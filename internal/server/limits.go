package server

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sync"
	"time"
)

type requestLimiter struct {
	slots   chan struct{}
	mu      sync.Mutex
	tokens  float64
	rate    int
	updated time.Time
}

func newRequestLimiter(concurrency, rate int) *requestLimiter {
	return &requestLimiter{slots: make(chan struct{}, concurrency), tokens: float64(rate), rate: rate, updated: time.Now()}
}

func (l *requestLimiter) acquire(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.After(l.updated) {
		l.tokens = min(float64(l.rate), l.tokens+now.Sub(l.updated).Seconds()*float64(l.rate))
		l.updated = now
	}
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

func (l *requestLimiter) release()    { <-l.slots }
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
		return 0, errors.New("local response exceeds size limit")
	}
	if !f.budget.reserve(int64(len(p))) {
		return 0, errors.New("temporary response budget exhausted")
	}
	n, err := f.file.Write(p)
	f.bytes += int64(n)
	f.budget.release(int64(len(p) - n))
	return n, err
}

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

func (w *idleResponseWriter) WriteHeader(status int) {
	_ = w.refreshWriteDeadline()
	w.ResponseWriter.WriteHeader(status)
}

func (w *idleResponseWriter) Write(p []byte) (int, error) {
	if err := w.refreshWriteDeadline(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(p)
}

func (w *idleResponseWriter) FlushError() error {
	if err := w.refreshWriteDeadline(); err != nil {
		return err
	}
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
