package server

import (
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// A fanout streams one read of a stored response entry to every concurrent
// requester of the same pack key: the pump goroutine reads the file once and
// distributes shared blocks to subscriber queues, so a fleet of identical
// requests costs one disk read instead of one per response. Memory is hard
// bounded: every block holds one slot of a fixed semaphore (fanoutPoolBlocks
// x fanoutBlockSize), block buffers come from a pooled free list reused
// across streams — and the pool is elastic: under a soft memory limit the
// collector drains it instead of counting idle buffers against the budget —
// while a subscriber whose bounded queue is full detaches and finishes by
// reading the stored file itself from its own offset, which also keeps
// detached transfers correct when the pump ends first.
const (
	fanoutBlockSize  = 64 << 10
	fanoutQueueDepth = 2
	fanoutPoolBlocks = 128
)

type fanoutRegistry struct {
	mu       sync.Mutex
	active   map[string]*fanout
	slots    chan struct{}
	buffers  sync.Pool
	shutdown chan struct{}
	workers  sync.WaitGroup
}

func (r *fanoutRegistry) init() {
	r.active = make(map[string]*fanout)
	r.slots = make(chan struct{}, fanoutPoolBlocks)
	r.buffers.New = func() any {
		buffer := make([]byte, fanoutBlockSize)
		return &buffer
	}
	r.shutdown = make(chan struct{})
}

// close stops every pump and waits for them. Subscribers are detached, not
// aborted: their responses continue from their own file reads, so closing
// the server never truncates an in-flight transfer that can serve itself.
func (r *fanoutRegistry) close() {
	r.mu.Lock()
	if r.shutdown == nil || r.isShutdown() {
		r.mu.Unlock()
		return
	}
	close(r.shutdown)
	r.mu.Unlock()
	r.workers.Wait()
}

func (r *fanoutRegistry) isShutdown() bool {
	select {
	case <-r.shutdown:
		return true
	default:
		return false
	}
}

// fanoutBlock is one shared slice of response bytes backed by a pooled
// buffer. The final holder of the reference count recycles the buffer and
// releases the slot; terminal EOF is not a block — the pump detaches every
// subscriber instead, and their fallback reads find the file's own end.
type fanoutBlock struct {
	data []byte
	refs int32
	reg  *fanoutRegistry
}

func (b *fanoutBlock) share() { atomic.AddInt32(&b.refs, 1) }
func (b *fanoutBlock) release() {
	if atomic.AddInt32(&b.refs, -1) == 0 {
		b.reg.recycle(b.data)
		<-b.reg.slots
	}
}

// recycle returns a buffer to the free list. The pooled pointers keep the
// list elastic: a GC under memory pressure empties it instead of letting
// idle buffers count against a soft memory limit.
func (r *fanoutRegistry) recycle(buf []byte) {
	buffer := buf[:cap(buf)]
	r.buffers.Put(&buffer)
}

type fanout struct {
	reg    *fanoutRegistry
	key    string
	path   string
	file   *os.File
	mu     sync.Mutex
	subs   map[*fanoutSub]struct{}
	sent   int64 // bytes already broadcast; subscribers may only join at zero
	stop   bool
	detach func() // counts backpressure detaches (Server metric)
}

// fanoutSub is one response's subscription. blocks, detached and closed are
// touched by the pump; cur, off, delivered and fb belong to the reader
// goroutine alone.
type fanoutSub struct {
	f        *fanout
	blocks   chan *fanoutBlock
	detached atomic.Bool
	closed   bool
	cur      *fanoutBlock
	off      int
	// prefix is the broadcast position at attach time: the subscriber
	// serves [0, prefix) from its own file reads and then continues with
	// the queued blocks, which start exactly at that position.
	prefix    int64
	delivered int64
	fb        *os.File
	fbAt      int64
}

// join returns a subscription that replays the stored entry identified by
// key, creating the broadcast when none is active. A subscriber that
// arrives after the pump advanced serves the prefix it missed from its own
// file reads and continues with the broadcast blocks from that position.
// file is the caller's open entry: it becomes the pump's source when a
// fanout is created, and is closed here when an existing fanout is joined.
func (r *fanoutRegistry) join(key, path string, file *os.File, detach func()) *fanoutSub {
	r.mu.Lock()
	if r.isShutdown() {
		r.mu.Unlock()
		return nil
	}
	if f, ok := r.active[key]; ok {
		r.mu.Unlock()
		if sub := f.attach(); sub != nil {
			file.Close()
			return sub
		}
		return nil
	}
	f := &fanout{reg: r, key: key, path: path, file: file, subs: make(map[*fanoutSub]struct{}), detach: detach}
	r.active[key] = f
	r.mu.Unlock()
	// Attach the creator before the pump can deliver or finish, so the
	// broadcast always has its first subscriber.
	sub := f.attach()
	r.workers.Add(1)
	go f.pump()
	return sub
}

func (f *fanout) attach() *fanoutSub {
	sub := &fanoutSub{f: f, blocks: make(chan *fanoutBlock, fanoutQueueDepth)}
	f.mu.Lock()
	if f.stop {
		f.mu.Unlock()
		return nil
	}
	sub.prefix = f.sent
	f.subs[sub] = struct{}{}
	f.mu.Unlock()
	return sub
}

func (f *fanout) pump() {
	defer f.reg.workers.Done()
	defer f.finish()
	for {
		if f.reg.isShutdown() {
			return
		}
		f.mu.Lock()
		empty := len(f.subs) == 0
		f.mu.Unlock()
		if empty {
			return
		}
		buf := f.acquire()
		if buf == nil {
			return
		}
		n, err := f.file.Read(buf)
		if n > 0 {
			// Ownership of the pooled buffer moves to the block.
			f.deliver(buf[:n])
		} else {
			f.reg.recycle(buf)
			<-f.reg.slots
		}
		if err != nil {
			return
		}
	}
}

// acquire reserves a block slot and returns a buffer to read into, or nil
// when the registry is shutting down.
func (f *fanout) acquire() []byte {
	select {
	case f.reg.slots <- struct{}{}:
	case <-f.reg.shutdown:
		return nil
	}
	if buffer, ok := f.reg.buffers.Get().(*[]byte); ok {
		return (*buffer)[:fanoutBlockSize]
	}
	return make([]byte, fanoutBlockSize)
}

// deliver shares one read block with every subscriber. A full queue detaches
// the subscriber: it finishes from its own file reads, and its queued blocks
// keep their reference counts until consumed or closed.
func (f *fanout) deliver(data []byte) {
	// The pump's own reference keeps the slot held until every subscriber
	// that took the block has finished with it (and returns it immediately
	// when none did).
	block := &fanoutBlock{data: data, refs: 1, reg: f.reg}
	f.mu.Lock()
	f.sent += int64(len(data))
	for sub := range f.subs {
		sub.offer(block)
	}
	f.mu.Unlock()
	block.release()
}

// offer enqueues a block for this subscriber. Caller holds fanout.mu. The
// reference is taken before the block becomes visible to the reader, so a
// fast consumer cannot release the slot before the count includes it.
func (s *fanoutSub) offer(block *fanoutBlock) bool {
	if s.closed || s.detached.Load() {
		return false
	}
	block.share()
	select {
	case s.blocks <- block:
		return true
	default:
		block.release()
		s.detached.Store(true)
		if s.f.detach != nil {
			s.f.detach()
		}
		return false
	}
}

// finish ends the broadcast: every subscriber is detached to its own file
// reads and the entry is removed from the registry so the next request for
// the same key starts a fresh pump. A nil sentinel wakes subscribers parked
// waiting for the next block; their queue is empty by definition, so the
// non-blocking send always lands.
func (f *fanout) finish() {
	_ = f.file.Close()
	f.mu.Lock()
	f.stop = true
	for sub := range f.subs {
		sub.detached.Store(true)
		select {
		case sub.blocks <- nil:
		default:
		}
	}
	f.mu.Unlock()
	f.reg.mu.Lock()
	if f.reg.active[f.key] == f {
		delete(f.reg.active, f.key)
	}
	f.reg.mu.Unlock()
}

// Read serves the subscriber from its current block, queued blocks, or —
// before the attach position or once detached — from the stored file at
// its delivered offset. A nil block is only a wakeup: the subscriber
// re-evaluates its detached state.
func (s *fanoutSub) Read(p []byte) (int, error) {
	if s.cur == nil && s.delivered < s.prefix {
		// Serve the pre-broadcast prefix from the file, never past the
		// attach position: the queued blocks continue exactly there.
		if remaining := s.prefix - s.delivered; int64(len(p)) > remaining {
			p = p[:remaining]
		}
		n, err := s.readFallback(p)
		if s.delivered >= s.prefix && s.fb != nil {
			_ = s.fb.Close()
			s.fb = nil
		}
		return n, err
	}
	if s.cur == nil && !s.detached.Load() {
		block := <-s.blocks
		if block != nil {
			s.cur, s.off = block, 0
		}
	}
	if s.cur != nil {
		n := copy(p, s.cur.data[s.off:])
		s.off += n
		s.delivered += int64(n)
		if s.off == len(s.cur.data) {
			s.cur.release()
			s.cur, s.off = nil, 0
		}
		return n, nil
	}
	if s.detached.Load() {
		return s.readFallback(p)
	}
	return 0, nil
}

func (s *fanoutSub) readFallback(p []byte) (int, error) {
	if s.fb == nil {
		file, err := os.Open(s.f.path)
		if err != nil {
			return 0, err
		}
		if _, err := file.Seek(s.delivered, io.SeekStart); err != nil {
			_ = file.Close()
			return 0, err
		}
		s.fb = file
		s.fbAt = s.delivered
	} else if s.fbAt != s.delivered {
		// A fallback handle opened for the prefix phase sits at the attach
		// position; re-sync it to the bytes the subscriber has served.
		if _, err := s.fb.Seek(s.delivered, io.SeekStart); err != nil {
			return 0, err
		}
		s.fbAt = s.delivered
	}
	n, err := s.fb.Read(p)
	s.delivered += int64(n)
	s.fbAt = s.delivered
	return n, err
}

// Close releases the subscription: queued and current blocks return their
// references, the subscriber unregisters, and a fallback file closes. It
// runs after the response finished reading, so no Read is in flight.
func (s *fanoutSub) Close() error {
	f := s.f
	f.mu.Lock()
	s.closed = true
	delete(f.subs, s)
	for s.blocks != nil {
		select {
		case block := <-s.blocks:
			if block != nil {
				block.release()
			}
		default:
			s.blocks = nil
		}
	}
	f.mu.Unlock()
	if s.cur != nil {
		s.cur.release()
		s.cur = nil
	}
	if s.fb != nil {
		err := s.fb.Close()
		s.fb = nil
		return err
	}
	return nil
}

// staticFileInfo reports the known entry size for subscribers, whose bytes
// arrive from the broadcast rather than a seekable file.
type staticFileInfo struct {
	name string
	size int64
}

func (i staticFileInfo) Name() string       { return i.name }
func (i staticFileInfo) Size() int64        { return i.size }
func (i staticFileInfo) Mode() os.FileMode  { return 0o444 }
func (i staticFileInfo) ModTime() time.Time { return time.Time{} }
func (i staticFileInfo) IsDir() bool        { return false }
func (i staticFileInfo) Sys() any           { return nil }
