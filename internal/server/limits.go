package server

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// admissionWait bounds how long a request queues for a slot or a rate
// token. Git does not retry a 503, so a burst that is over the limits for
// a fraction of a second would otherwise fail clones and CI jobs; the
// waiting room holds one burst, which the rate can serve within the wait.
const admissionWait = time.Second

type requestLimiter struct {
	slots chan struct{}
	// queue is the waiting room: at most one burst waits, and requests
	// beyond it are rejected at once instead of piling up connections.
	queue   chan struct{}
	mu      sync.Mutex
	tokens  float64
	rate    int
	updated time.Time
	// changed is closed and recreated on every release, waking waiters
	// that queue for a slot.
	changed chan struct{}
	wait    time.Duration
}

func newRequestLimiter(concurrency, rate int) *requestLimiter {
	return &requestLimiter{slots: make(chan struct{}, concurrency), queue: make(chan struct{}, rate), tokens: float64(rate), rate: rate, updated: time.Now(), changed: make(chan struct{}), wait: admissionWait}
}

// takeAcquire admits a request at once when both a token and a slot are free.
func (l *requestLimiter) tryAcquire(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.refillLocked(now)
	if l.tokens < 1 {
		return false
	}
	select {
	case l.slots <- struct{}{}:
		l.tokens--
		return true
	default:
		return false
	}
}

func (l *requestLimiter) refillLocked(now time.Time) {
	if now.After(l.updated) {
		l.tokens = min(float64(l.rate), l.tokens+now.Sub(l.updated).Seconds()*float64(l.rate))
		l.updated = now
	}
}

// acquire admits a request, waiting up to the admission wait for capacity.
// A ctx that ends while waiting — the client disconnected — gives up.
func (l *requestLimiter) acquire(ctx context.Context) bool {
	now := time.Now()
	if l.tryAcquire(now) {
		return true
	}
	select {
	case l.queue <- struct{}{}:
		defer func() { <-l.queue }()
	default:
		return false
	}
	deadline := now.Add(l.wait)
	for {
		now = time.Now()
		if l.tryAcquire(now) {
			return true
		}
		if !now.Before(deadline) {
			return false
		}
		l.mu.Lock()
		changed := l.changed
		var wake <-chan time.Time
		// Token-bound: wake when the next token has accrued. Slot-bound:
		// only a release changes anything.
		if l.tokens < 1 {
			delay := time.Duration((1 - l.tokens) / float64(l.rate) * float64(time.Second))
			timer := time.NewTimer(min(delay, time.Until(deadline)))
			wake = timer.C
			defer timer.Stop()
		}
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return false
		case <-changed:
		case <-wake:
		}
	}
}

func (l *requestLimiter) release() {
	<-l.slots
	l.mu.Lock()
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}
func (l *requestLimiter) active() int { return len(l.slots) }

type responseBudget struct {
	mu          sync.Mutex
	used, limit int64
}

func (b *responseBudget) reserve(n int64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > b.limit-b.used {
		return false
	}
	b.used += n
	return true
}

func (b *responseBudget) release(n int64) {
	b.mu.Lock()
	b.used -= n
	b.mu.Unlock()
}

func (b *responseBudget) bytes() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.used
}

type responseFile struct {
	file    *os.File
	stream  *fanoutSub // optional: broadcast subscription instead of a private file
	size    int64      // entry size when stream is set
	budget  *responseBudget
	limit   int64
	bytes   int64
	persist bool   // cached packs are shared entries, not spool files
	unpin   func() // optional: releases the scope pin when streaming ends
	once    sync.Once
	err     error
}

func (f *responseFile) Name() string {
	if f.stream != nil {
		return f.stream.f.path
	}
	return f.file.Name()
}
func (f *responseFile) Read(p []byte) (int, error) {
	if f.stream != nil {
		return f.stream.Read(p)
	}
	return f.file.Read(p)
}
func (f *responseFile) Seek(offset int64, whence int) (int64, error) {
	if f.stream != nil {
		// Broadcast subscriptions have no file descriptor to reposition.
		return 0, errors.New("broadcast stream is not seekable")
	}
	return f.file.Seek(offset, whence)
}
func (f *responseFile) Stat() (os.FileInfo, error) {
	if f.stream != nil {
		return staticFileInfo{name: f.Name(), size: f.size}, nil
	}
	return f.file.Stat()
}

func (f *responseFile) Write(p []byte) (int, error) {
	if int64(len(p)) > f.limit-f.bytes {
		return 0, responseLimitError{reason: "response_size_limit"}
	}
	if !f.budget.reserve(int64(len(p))) {
		return 0, responseLimitError{reason: "temporary_response_capacity"}
	}
	n, err := f.file.Write(p)
	f.bytes += int64(n)
	f.budget.release(int64(len(p) - n))
	return n, err
}

type responseLimitError struct{ reason string }

func (e responseLimitError) Error() string {
	if e.reason == "response_size_limit" {
		return "local response exceeds size limit"
	}
	return "temporary response budget exhausted"
}

func (e responseLimitError) LogKind() string { return e.reason }

// Close holds the budget until the response file has been removed. Files
// served from the response pack cache are shared and stay on disk; their
// scope pin is released once the transfer has finished reading the file.
// Broadcast subscriptions close their subscription instead of the pump's
// shared file descriptor.
func (f *responseFile) Close() error {
	f.once.Do(func() {
		if f.stream != nil {
			f.err = f.stream.Close()
		} else {
			f.err = f.file.Close()
		}
		if f.unpin != nil {
			f.unpin()
		}
		if f.persist {
			return
		}
		if err := os.Remove(f.Name()); err != nil && !os.IsNotExist(err) {
			f.err = errors.Join(f.err, err)
			return
		}
		f.budget.release(f.bytes)
	})
	return f.err
}

// idleResponseWriter bounds a stalled write while allowing long, progressing transfers.
type idleResponseWriter struct {
	http.ResponseWriter
	controller *http.ResponseController
	timeout    time.Duration
	ctx        context.Context
	status     atomic.Int32
	bytes      atomic.Int64
	// writeErr is the first failed write to the client; upstreamErr is the
	// first failed read of an upstream body streamed through the passthrough.
	// ReverseProxy aborts on either without reporting which side failed.
	writeErr    error
	upstreamErr func() error
}

func (w *idleResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *idleResponseWriter) refreshWriteDeadline() error {
	if w.ctx != nil && w.ctx.Err() != nil {
		return w.ctx.Err()
	}
	err := w.controller.SetWriteDeadline(time.Now().Add(w.timeout))
	if errors.Is(err, http.ErrNotSupported) {
		return nil
	}
	return err
}

// refreshReadDeadline bounds the next read of the request body. The server's
// absolute read timeout is left off — it cut off large pushes that were
// making progress — so a stalled or slow-feeding upload is bounded here by
// the idle timeout instead, one deadline per read.
func (w *idleResponseWriter) refreshReadDeadline() {
	_ = w.controller.SetReadDeadline(time.Now().Add(w.timeout))
}

func (w *idleResponseWriter) WriteHeader(status int) {
	if status >= 200 || status == http.StatusSwitchingProtocols {
		w.status.CompareAndSwap(0, int32(status))
	}
	_ = w.refreshWriteDeadline()
	w.ResponseWriter.WriteHeader(status)
}

func (w *idleResponseWriter) Write(p []byte) (int, error) {
	if err := w.refreshWriteDeadline(); err != nil {
		return 0, err
	}
	w.status.CompareAndSwap(0, http.StatusOK)
	n, err := w.ResponseWriter.Write(p)
	w.bytes.Add(int64(n))
	if err != nil && w.writeErr == nil {
		w.writeErr = err
	}
	return n, err
}

// interrupted reports why a passthrough transfer was aborted after its
// headers were committed: the side that failed — the client stopped
// accepting, the upstream stopped sending — and, when neither reported an
// error, the request deadline.
func (w *idleResponseWriter) interrupted() (string, error) {
	if w.writeErr != nil {
		return "client_write", w.writeErr
	}
	if w.upstreamErr != nil {
		if err := w.upstreamErr(); err != nil {
			return "upstream_read", err
		}
	}
	if w.ctx != nil && w.ctx.Err() != nil {
		return "deadline", w.ctx.Err()
	}
	return "unknown", nil
}

// idleRequestBody refreshes the connection's read deadline before every
// read; see refreshReadDeadline.
type idleRequestBody struct {
	io.ReadCloser
	output *idleResponseWriter
}

func (b *idleRequestBody) Read(p []byte) (int, error) {
	b.output.refreshReadDeadline()
	return b.ReadCloser.Read(p)
}

func (w *idleResponseWriter) FlushError() error {
	if err := w.refreshWriteDeadline(); err != nil {
		return err
	}
	w.status.CompareAndSwap(0, http.StatusOK)
	return w.controller.Flush()
}

func (w *idleResponseWriter) requestDeadline(r *http.Request, timeout time.Duration) (*http.Request, func()) {
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	w.ctx = ctx
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = w.controller.SetReadDeadline(time.Now())
		_ = w.controller.SetWriteDeadline(time.Now())
		close(done)
	})
	return r.WithContext(ctx), func() {
		if !stop() {
			<-done
		}
		cancel()
	}
}
