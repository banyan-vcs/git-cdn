package cache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/nativegit"
	"github.com/chiyutianyi/git-cdn/internal/protocol"
)

type Store struct {
	Root          string
	Tmp           string
	Git           *nativegit.Runner
	lock          *os.File
	mu            sync.Mutex
	entries       map[string]*entry
	maxBytes      int64
	usedBytes     int64
	reservedBytes int64
	packCache     atomic.Int64
	evictions     uint64
	evictedBytes  uint64
	repack        RepackOptions
	repackQueue   chan string
	maintenance   context.Context
	cancel        context.CancelFunc
	workers       sync.WaitGroup
	closeOnce     sync.Once
	closeErr      error
	closed        bool
	repacks       uint64
	repackFailed  uint64
	repackSkipped uint64
	gcs           uint64
	gcFailed      uint64
	gcSkipped     uint64
	reconciles    uint64
	rootsRetired  uint64
}

func Open(root string, git *nativegit.Runner, maxBytes int64, options ...RepackOptions) (*Store, error) {
	if maxBytes <= 0 {
		return nil, fmt.Errorf("cache byte limit must be positive")
	}
	root, err := filepath.Abs(root)
	if err != nil || strings.ContainsAny(root, "\r\n") {
		return nil, fmt.Errorf("invalid cache directory")
	}
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	lock, err := lockDirectory(filepath.Join(root, ".lock"))
	if err != nil {
		return nil, err
	}
	s := &Store{Root: root, Tmp: filepath.Join(root, "tmp"), Git: git, lock: lock, entries: make(map[string]*entry), maxBytes: maxBytes}
	s.repack = RepackOptions{PackThreshold: 32, Timeout: 2 * time.Minute}
	if len(options) > 0 {
		s.repack = options[0]
	}
	if s.repack.GCInterval == 0 {
		s.repack.GCInterval = 24 * time.Hour
	}
	if s.repack.GCRetention == 0 {
		s.repack.GCRetention = MinimumGCRetention
	}
	if s.repack.PackThreshold < 2 || s.repack.Timeout <= 0 || s.repack.GCInterval <= 0 || s.repack.GCRetention < MinimumGCRetention {
		s.Close()
		return nil, fmt.Errorf("invalid repack options")
	}
	if err = ensureDirectory(s.Tmp); err != nil {
		s.Close()
		return nil, err
	}
	// The directory lock excludes live requests while removing files left by a crash.
	files, err := os.ReadDir(s.Tmp)
	if err == nil {
		for _, file := range files {
			if err = os.RemoveAll(filepath.Join(s.Tmp, file.Name())); err != nil {
				break
			}
		}
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	if err := s.migrateLegacy(); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate repository caches: %w", err)
	}
	if err := os.Remove(filepath.Join(root, ".scope-key")); err != nil && !os.IsNotExist(err) {
		s.Close()
		return nil, err
	}
	if err := s.loadEntries(); err != nil {
		s.Close()
		return nil, err
	}
	if err := s.makeRoomLocked(0); err != nil {
		s.Close()
		return nil, err
	}
	if git != nil {
		s.maintenance, s.cancel = context.WithCancel(context.Background())
		s.repackQueue = make(chan string, 64)
		s.workers.Add(1)
		for _, e := range s.entries {
			s.queueRepackLocked(e)
		}
		go s.maintain()
	}
	return s, nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		// Flag closure under mu so a Reconcile racing this Close either
		// registers in workers before the Wait below or returns at once.
		s.mu.Lock()
		s.closed = true
		s.mu.Unlock()
		if s.cancel != nil {
			s.cancel()
		}
		// Join the maintenance worker and every in-flight reconciliation
		// goroutine: their git children are killed by the cancel above and
		// waited for, so no writes land in the cache directory afterwards.
		s.workers.Wait()
		// Persist suppressed recency stamps so a clean shutdown leaves the
		// true last-touch ordering for the next start.
		type stamp struct {
			key string
			at  time.Time
		}
		var due []stamp
		s.mu.Lock()
		for _, e := range s.entries {
			if e.accessDirty && e.size > 0 {
				e.accessDirty = false
				due = append(due, stamp{key: e.key, at: e.accessed})
			}
		}
		s.mu.Unlock()
		for _, item := range due {
			s.flushAccess(filepath.Join(s.Root, "repos", item.key), item.at, true)
		}
		s.closeErr = s.lock.Close()
	})
	return s.closeErr
}

func (s *Store) Key(identity string) string {
	identityHash := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(identityHash[:])
}

// SetPackCacheBytes configures the per-repository response-pack budget.
// Zero disables response-pack caching; it must be set before serving.
func (s *Store) SetPackCacheBytes(limit int64) {
	if limit < 0 {
		limit = 0
	}
	s.packCache.Store(limit)
}

// PackCacheBytes reports the configured per-repository response-pack budget.
func (s *Store) PackCacheBytes() int64 { return s.packCache.Load() }

type Lease struct {
	Dir   string
	store *Store
	entry *entry
	// shared marks a reader lease: it excludes concurrent writers but not
	// other readers, and it never mutates repository state.
	shared bool
}

// Acquire takes the exclusive repository lease for key. Hydration, ref
// reconciliation and maintenance writers require it.
func (s *Store) Acquire(ctx context.Context, key string) (*Lease, error) {
	return s.acquire(ctx, key, false)
}

// AcquireShared takes a reader lease for key. Concurrent readers run in
// parallel; writers wait until the last reader releases.
func (s *Store) AcquireShared(ctx context.Context, key string) (*Lease, error) {
	return s.acquire(ctx, key, true)
}

func (s *Store) acquire(ctx context.Context, key string, shared bool) (*Lease, error) {
	if !validKey(key) {
		return nil, fmt.Errorf("invalid cache key")
	}
	s.mu.Lock()
	e := s.entries[key]
	if e == nil {
		e = newEntry(key)
		s.entries[key] = e
	}
	// Waiting callers also pin the entry so eviction cannot replace their gate.
	e.users++
	if !shared {
		e.writers++
	}
	for !e.gateReady(shared) {
		// A waiting request preempts long maintenance: cancel it so the
		// queued reader or writer can proceed; maintenance retries later.
		if cancel := e.cancelMaint; cancel != nil {
			cancel()
		}
		changed := e.changed
		s.mu.Unlock()
		select {
		case <-changed:
			s.mu.Lock()
		case <-ctx.Done():
			s.mu.Lock()
			if !shared {
				e.writers--
			}
			e.users--
			if e.users == 0 && e.size == 0 {
				delete(s.entries, e.key)
			}
			s.mu.Unlock()
			return nil, ctx.Err()
		}
	}
	e.takeGate(shared)
	e.accessed = time.Now()
	s.mu.Unlock()
	return &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, entry: e, shared: shared}, nil
}

// reconcileHold is a test seam that pauses a reconciliation goroutine after
// it takes its lease, so tests can observe Close joining it deterministically.
var reconcileHold func()

// Reconcile refreshes the scope's storage refs to snapshot on a background
// exclusive lease. It never blocks or fails the caller; reconciliation
// catches up on the next trigger when the lease or the store is gone. The
// goroutine joins the store's workers, so Close waits for a reconciliation
// still holding its lease instead of racing its writes with removal of the
// cache directory. Triggers are debounced — the snapshot fingerprint must
// drift from the last applied one, or hydrations must accumulate — and the
// lease is taken only while the scope is completely idle, so reconciliation
// never queues behind (and blocks) fresh readers; a queued request can also
// preempt it through the maintenance cancellation hook.
func (s *Store) Reconcile(key string, snapshot protocol.Snapshot) {
	if s.Git == nil || s.maintenance == nil {
		return
	}
	fp := snapshot.Fingerprint()
	s.mu.Lock()
	e := s.entries[key]
	if e == nil || e.size == 0 || s.closed {
		s.mu.Unlock()
		return
	}
	if fp == e.reconcileFP && e.hydrateSince < reconcileHydrationDelta {
		s.mu.Unlock()
		return
	}
	if !e.reconcileFailed.IsZero() && time.Since(e.reconcileFailed) < reconcileFailureBackoff {
		s.mu.Unlock()
		return
	}
	s.workers.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.workers.Done()
		for attempt := 0; attempt < 3; attempt++ {
			ctx, cancel := context.WithTimeout(s.maintenance, time.Minute)
			lease := s.acquireIdle(key, e, cancel)
			if lease == nil {
				cancel()
				return
			}
			if hold := reconcileHold; hold != nil {
				hold()
			}
			err := lease.SyncRefs(ctx, snapshot)
			preempted := ctx.Err() == context.Canceled
			cancel()
			s.mu.Lock()
			e.cancelMaint = nil
			// A foreground request cancels reconciliation to make progress.
			// Its newer snapshot must not inherit failure backoff from that cancellation.
			if err != nil && !preempted {
				e.reconcileFailed = time.Now()
			}
			s.mu.Unlock()
			lease.Release()
			if err == nil || !preempted || attempt == 2 {
				return
			}
			// Preempted by a queued request: that request re-arms the
			// trigger when its own generation path runs Reconcile again,
			// but an aftermath of pure pack hits would not, so retry a
			// bounded number of times before staying due.
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

// acquireIdle takes the exclusive gate for background reconciliation only
// when the scope is completely idle — no readers, writers or waiters — and
// registers maintenance preemption so a queued request cancels the work
// instead of waiting behind it. A busy scope keeps the trigger due, and the
// next request retries. The brief retry loop absorbs the microseconds in
// which the spawning request path has not yet released its own lease.
func (s *Store) acquireIdle(key string, e *entry, cancel context.CancelFunc) *Lease {
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		if s.closed || s.entries[key] != e {
			s.mu.Unlock()
			return nil
		}
		if e.users == 0 && !e.writer && e.readers == 0 {
			e.users++
			e.writer = true
			e.cancelMaint = cancel
			s.mu.Unlock()
			return &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, entry: e}
		}
		s.mu.Unlock()
		if time.Now().After(deadline) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *Lease) Release() {
	l.release(true)
}

// Pin marks the scope for key as in use without holding its gate, so
// eviction cannot remove it while the caller streams its files. The
// returned function releases the pin and refreshes scope recency; it is
// nil when the scope is absent, in which case there is nothing to protect
// and the caller falls back to the gated path.
func (s *Store) Pin(key string) func() {
	if !validKey(key) {
		return nil
	}
	s.mu.Lock()
	e := s.entries[key]
	if e == nil {
		s.mu.Unlock()
		return nil
	}
	e.users++
	e.accessed = time.Now()
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		e.users--
		e.accessed = time.Now()
		at := e.accessed
		flush := e.claimAccessFlushLocked(at)
		if e.users == 0 && e.size == 0 {
			delete(s.entries, e.key)
			_ = os.Remove(filepath.Join(s.Root, "repos", e.key))
		}
		s.mu.Unlock()
		s.flushAccess(filepath.Join(s.Root, "repos", e.key), at, flush)
	}
}

// claimAccessFlushLocked reports whether the persisted recency stamp is
// due and reserves the flush so concurrent releases do not duplicate it.
// A suppressed flush marks the entry dirty: within a process LRU uses the
// in-memory timestamp, and Close persists dirty stamps so a clean restart
// still sees the true last-touch ordering. Caller holds Store.mu.
func (e *entry) claimAccessFlushLocked(now time.Time) bool {
	if e.size <= 0 {
		return false
	}
	if now.Sub(e.accessFlushed) < accessFlushInterval {
		e.accessDirty = true
		return false
	}
	e.accessFlushed = now
	e.accessDirty = false
	return true
}

// flushAccess persists a recency stamp outside the store mutex: the file
// is last-writer-wins per scope, and holding the global lock across disk
// writes serialized every concurrent response.
func (s *Store) flushAccess(dir string, at time.Time, due bool) {
	if !due {
		return
	}
	access := filepath.Join(dir, ".access")
	if err := os.WriteFile(access, nil, 0600); err == nil {
		_ = os.Chtimes(access, at, at)
	}
}

func (l *Lease) release(schedule bool) {
	s := l.store
	e := l.entry
	now := time.Now()
	// Writers rescan the scope size while still holding its exclusive
	// gate, before taking the global mutex: the walk no longer blocks
	// every other scope's gate operations. Readers skip the rescan.
	size, haveSize := int64(0), false
	if !l.shared {
		if scanned, err := directorySize(filepath.Dir(l.Dir)); err == nil {
			size, haveSize = scanned, true
		}
	}
	s.mu.Lock()
	e.accessed = now
	if haveSize {
		s.usedBytes += size - e.size
		e.size = size
	}
	// Claim after the size update: a writer's first release publishes the
	// scope size, and that release must also stamp recency.
	flush := e.claimAccessFlushLocked(now)
	e.dropGate(l.shared)
	e.signalGate()
	e.users--
	// Any overage from interrupted metadata writes is reclaimed when leases end.
	_ = s.makeRoomLocked(0)
	if e.users == 0 && e.size == 0 {
		delete(s.entries, e.key)
		_ = os.Remove(filepath.Dir(l.Dir))
	}
	if schedule && e.users == 0 && s.entries[e.key] == e {
		s.queueRepackLocked(e)
	}
	s.mu.Unlock()
	s.flushAccess(filepath.Dir(l.Dir), now, flush)
}
