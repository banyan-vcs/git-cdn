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
	repacks       uint64
	repackFailed  uint64
	repackSkipped uint64
	gcs           uint64
	gcFailed      uint64
	gcSkipped     uint64
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
		if s.cancel != nil {
			s.cancel()
		}
		s.workers.Wait()
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

// Reconcile refreshes the scope's storage refs to snapshot on a background
// exclusive lease. It never blocks or fails the caller; reconciliation
// catches up on the next writer when the lease or the store is gone.
func (s *Store) Reconcile(key string, snapshot protocol.Snapshot) {
	if s.Git == nil || s.maintenance == nil {
		return
	}
	s.mu.Lock()
	e := s.entries[key]
	known := e != nil && e.size > 0
	s.mu.Unlock()
	if !known {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(s.maintenance, time.Minute)
		defer cancel()
		lease, err := s.Acquire(ctx, key)
		if err != nil {
			return
		}
		defer lease.Release()
		_ = lease.SyncRefs(ctx, snapshot)
	}()
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
		if e.size > 0 {
			access := filepath.Join(s.Root, "repos", e.key, ".access")
			if err := os.WriteFile(access, nil, 0600); err == nil {
				_ = os.Chtimes(access, e.accessed, e.accessed)
			}
		}
		if e.users == 0 && e.size == 0 {
			delete(s.entries, e.key)
			_ = os.Remove(filepath.Join(s.Root, "repos", e.key))
		}
		s.mu.Unlock()
	}
}

func (l *Lease) release(schedule bool) {
	s := l.store
	s.mu.Lock()
	e := l.entry
	e.accessed = time.Now()
	if l.shared {
		// Readers never mutate the repository: refresh recency without
		// rescanning unchanged bytes.
		if e.size > 0 {
			access := filepath.Join(filepath.Dir(l.Dir), ".access")
			if err := os.WriteFile(access, nil, 0600); err == nil {
				_ = os.Chtimes(access, e.accessed, e.accessed)
			}
		}
	} else if size, err := directorySize(filepath.Dir(l.Dir)); err == nil {
		s.usedBytes += size - e.size
		e.size = size
		if e.size > 0 {
			access := filepath.Join(filepath.Dir(l.Dir), ".access")
			if err := os.WriteFile(access, nil, 0600); err == nil {
				_ = os.Chtimes(access, e.accessed, e.accessed)
			}
		}
	}
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
}
