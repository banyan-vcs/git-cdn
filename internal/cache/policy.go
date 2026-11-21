package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

var ErrCapacity = errors.New("repository cache capacity unavailable")

// The per-scope gate is a reader/writer lock expressed through fields guarded
// by Store.mu: readers add to readers, hydration/ref/repack writers hold
// writer, and waiting writers count in writers so fresh readers cannot
// starve them. changed is closed and recreated whenever the state moves;
// waiters select on it alongside their context.
type entry struct {
	key         string
	readers     int
	writers     int
	writer      bool
	changed     chan struct{}
	cancelMaint context.CancelFunc
	packMu      sync.Mutex
	users       int
	size        int64
	accessed    time.Time
	// accessFlushed is the last persisted .access stamp; recency writes
	// are throttled to accessFlushInterval because LRU needs seconds-level
	// granularity, not one disk write per request. accessDirty marks a
	// suppressed flush so Close persists the final ordering.
	accessFlushed  time.Time
	accessDirty    bool
	repackPending  bool
	refsObservedAt time.Time
	lastGC         time.Time
	// packs indexes the response-pack cache (key hex -> published size) so
	// warm lookups Stat one file instead of scanning the directory. It is
	// guarded by Store.mu and rebuilt at startup.
	packs map[string]int64
	// Reconciliation debounce: reconcileFP is the fingerprint of the last
	// snapshot SyncRefs applied, hydrateSince counts hydrations since that
	// application (the accumulation gate for snapshots that never drift),
	// and reconcileFailedAt backs off retries after a failed attempt.
	reconcileFP     [32]byte
	hydrateSince    int
	reconcileFailed time.Time
}

// reconcileHydrationDelta bounds root accumulation between reconciles when
// the upstream ref picture never changes: fleets fetching unadvertised
// SHAs hydrate one root per fetch without moving any advertised ref, so
// after this many hydrations a reconcile is due even though the snapshot
// fingerprint is unchanged.
const reconcileHydrationDelta = 64

// reconcileFailureBackoff throttles reconciliation retries after a failed
// SyncRefs attempt, so a persistently failing scope cannot turn every
// request into another background git run.
const reconcileFailureBackoff = 30 * time.Second

func newEntry(key string) *entry {
	return &entry{key: key, changed: make(chan struct{}), lastGC: time.Now()}
}

// gateReady reports whether the requested mode can be taken now. Caller
// holds Store.mu.
func (e *entry) gateReady(shared bool) bool {
	if shared {
		return !e.writer && e.writers == 0
	}
	return !e.writer && e.readers == 0
}

func (e *entry) takeGate(shared bool) {
	if shared {
		e.readers++
		return
	}
	e.writers--
	e.writer = true
}

func (e *entry) dropGate(shared bool) {
	if shared {
		e.readers--
		return
	}
	e.writer = false
}

// signalGate wakes all waiters. Caller holds Store.mu.
func (e *entry) signalGate() {
	close(e.changed)
	e.changed = make(chan struct{})
}

type Stats struct {
	Bytes         int64
	MaxBytes      int64
	ReservedBytes int64
	Entries       int
	Evictions     uint64
	EvictedBytes  uint64
	Repacks       uint64
	RepackFailed  uint64
	RepackSkipped uint64
	GCs           uint64
	GCFailed      uint64
	GCSkipped     uint64
	Reconciles    uint64
	RootsRetired  uint64
}

func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Stats{Bytes: s.usedBytes, MaxBytes: s.maxBytes, ReservedBytes: s.reservedBytes, Entries: len(s.entries), Evictions: s.evictions, EvictedBytes: s.evictedBytes, Repacks: s.repacks, RepackFailed: s.repackFailed, RepackSkipped: s.repackSkipped, GCs: s.gcs, GCFailed: s.gcFailed, GCSkipped: s.gcSkipped, Reconciles: s.reconciles, RootsRetired: s.rootsRetired}
}

func digest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validKey(key string) bool {
	return digest(key)
}

// Count logical regular-file bytes without following links outside a managed cache.
func directorySize(path string) (int64, error) {
	var size int64
	err := filepath.WalkDir(path, func(path string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, err := d.Info()
			if os.IsNotExist(err) {
				return nil
			}
			if err != nil {
				return err
			}
			size += info.Size()
		}
		return nil
	})
	return size, err
}

func (s *Store) loadEntries() error {
	repos := filepath.Join(s.Root, "repos")
	if err := ensureDirectory(repos); err != nil {
		return err
	}
	identities, err := os.ReadDir(repos)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, identity := range identities {
		if !identity.IsDir() || !digest(identity.Name()) {
			continue
		}
		key := identity.Name()
		path := filepath.Join(repos, key)
		sweepStaging(filepath.Join(path, "packcache"))
		size, err := directorySize(path)
		if err != nil {
			return err
		}
		info, err := os.Stat(filepath.Join(path, ".access"))
		if os.IsNotExist(err) {
			info, err = identity.Info()
		}
		if err != nil {
			return err
		}
		lastGC, err := readGCTime(filepath.Join(path, "repo.git"), info.ModTime())
		if err != nil {
			return err
		}
		s.entries[key] = &entry{key: key, changed: make(chan struct{}), size: size, accessed: info.ModTime(), accessFlushed: info.ModTime(), lastGC: lastGC, packs: scanPackIndex(filepath.Join(path, "packcache"))}
		s.usedBytes += size
	}
	return nil
}

// accessFlushInterval bounds persisted recency-stamp writes: within one,
// releases only update the in-memory timestamp. LRU decisions use
// in-memory recency; the stamp only orders cold scopes after a restart.
const accessFlushInterval = 30 * time.Second

// scanPackIndex rebuilds the response-pack index from published entry
// names (<key hex>-<size>). Staging leftovers were swept before this runs.
func scanPackIndex(dir string) map[string]int64 {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var index map[string]int64
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || filepath.Ext(name) == ".staging" {
			continue
		}
		dash := strings.LastIndexByte(name, '-')
		if dash <= 0 {
			continue
		}
		size, err := strconv.ParseInt(name[dash+1:], 10, 64)
		if err != nil || size <= 0 {
			continue
		}
		if index == nil {
			index = make(map[string]int64)
		}
		index[name[:dash]] = size
	}
	return index
}

func ensureDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("managed cache path is not a directory")
	}
	return nil
}

func (s *Store) ensureScope(key string) error {
	for _, path := range []string{filepath.Join(s.Root, "repos"), filepath.Join(s.Root, "repos", key)} {
		if err := ensureDirectory(path); err != nil {
			return err
		}
	}
	return nil
}

// removeScope is swapped out by tests to simulate eviction failures.
var removeScope = os.RemoveAll

// Caller holds mu. Only unpinned entries may be removed, including pending waiters.
func (s *Store) makeRoomLocked(additional int64) error {
	if additional < 0 || additional > s.maxBytes {
		return ErrCapacity
	}
	if s.usedBytes+s.reservedBytes <= s.maxBytes-additional {
		return nil
	}
	var candidates []*entry
	for _, e := range s.entries {
		if e.users == 0 {
			candidates = append(candidates, e)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].accessed.Equal(candidates[j].accessed) {
			return candidates[i].key < candidates[j].key
		}
		return candidates[i].accessed.Before(candidates[j].accessed)
	})
	var evictErr error
	for _, e := range candidates {
		path := filepath.Join(s.Root, "repos", e.key)
		if err := removeScope(path); err != nil {
			// A busy candidate — for example a response still streaming on
			// Windows — must not wedge admission for the rest of the cache.
			// Evict the remaining candidates and report the failure only
			// when capacity is still short.
			if evictErr == nil {
				evictErr = fmt.Errorf("evict repository cache: %w", err)
			}
			continue
		}
		s.usedBytes -= e.size
		s.evictions++
		s.evictedBytes += uint64(e.size)
		delete(s.entries, e.key)
		if s.usedBytes+s.reservedBytes <= s.maxBytes-additional {
			return nil
		}
	}
	if evictErr != nil {
		return evictErr
	}
	return ErrCapacity
}

type reservation struct {
	lease *Lease
	bytes int64
}

func (l *Lease) reserve(additional int64) (*reservation, error) {
	s := l.store
	s.mu.Lock()
	defer s.mu.Unlock()
	additional = max(additional, 0)
	if l.entry.size > s.maxBytes-additional {
		return nil, ErrCapacity
	}
	if err := s.makeRoomLocked(additional); err != nil {
		return nil, err
	}
	s.reservedBytes += additional
	return &reservation{lease: l, bytes: additional}, nil
}

func (r *reservation) release() {
	s := r.lease.store
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reservedBytes -= r.bytes
	if size, err := directorySize(filepath.Dir(r.lease.Dir)); err == nil {
		s.usedBytes += size - r.lease.entry.size
		r.lease.entry.size = size
	} else {
		// Keep the admission estimate charged when the filesystem cannot be measured.
		s.usedBytes += r.bytes
		r.lease.entry.size += r.bytes
	}
}
