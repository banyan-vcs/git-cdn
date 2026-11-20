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
}

func (s *Store) Acquire(ctx context.Context, key string) (*Lease, error) {
	s.mu.Lock()
	if !validKey(key) {
		s.mu.Unlock()
		return nil, fmt.Errorf("invalid cache key")
	}
	e := s.entries[key]
	if e == nil {
		e = &entry{key: key, gate: make(chan struct{}, 1), lastGC: time.Now()}
		s.entries[key] = e
	}
	// Waiting callers also pin the entry so eviction cannot replace their gate.
	e.users++
	s.mu.Unlock()
	select {
	case e.gate <- struct{}{}:
		s.mu.Lock()
		e.accessed = time.Now()
		s.mu.Unlock()
		return &Lease{Dir: filepath.Join(s.Root, "repos", key, "repo.git"), store: s, entry: e}, nil
	case <-ctx.Done():
		s.mu.Lock()
		e.users--
		if e.users == 0 && e.size == 0 {
			delete(s.entries, e.key)
		}
		s.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (l *Lease) Release() {
	l.release(true)
}

func (l *Lease) release(schedule bool) {
	s := l.store
	s.mu.Lock()
	e := l.entry
	e.accessed = time.Now()
	if size, err := directorySize(filepath.Dir(l.Dir)); err == nil {
		s.usedBytes += size - e.size
		e.size = size
	}
	if e.size > 0 {
		access := filepath.Join(filepath.Dir(l.Dir), ".access")
		if err := os.WriteFile(access, nil, 0600); err == nil {
			_ = os.Chtimes(access, e.accessed, e.accessed)
		}
	}
	e.users--
	<-e.gate
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
