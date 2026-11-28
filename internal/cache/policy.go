package cache

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

var (
	ErrCapacity = errors.New("repository cache capacity unavailable")
	// ErrHydrationBackoff reports that missing roots were not fetched
	// because the scope's previous hydration failed inside its backoff
	// window. The request passes through instead of repeating the attempt.
	ErrHydrationBackoff = errors.New("cache hydration is backing off after a failure")
)

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
	// regularPacks counts object packs that geometric repack may rewrite
	// (cruft packs excluded). Writers refresh it from their release rescan,
	// so the maintenance queue decides from memory instead of a directory
	// listing under the store mutex.
	regularPacks int
	// accessFlushed is the last persisted .access stamp; recency writes
	// are throttled to accessFlushInterval because LRU needs seconds-level
	// granularity, not one disk write per request. accessDirty marks a
	// suppressed flush so Close persists the final ordering.
	accessFlushed  time.Time
	accessDirty    bool
	repackPending  bool
	refsObservedAt time.Time
	lastGC         time.Time
	// packs indexes the response-pack cache (key hex -> entry) so warm
	// lookups Stat one file instead of scanning the directory. It is guarded
	// by Store.mu and rebuilt at startup.
	packs map[string]*packEntry
	// Reconciliation debounce: reconcileFP is the fingerprint of the last
	// snapshot SyncRefs applied, hydrateSince counts hydrations since that
	// application (the accumulation gate for snapshots that never drift),
	// and reconcile spaces out retries after a failed attempt.
	reconcileFP  [32]byte
	hydrateSince int
	reconcile    Backoff
	// reconcileWant is the newest snapshot a served request asked to apply,
	// and reconcileWaiting is set while a background run exists that will
	// apply it: a trigger arriving meanwhile only replaces the snapshot.
	reconcileWant    protocol.Snapshot
	reconcileWaiting bool
	// site is the upstream alias the scope was last served for, recorded
	// in .site so that a restart keeps the per-site split.
	site string
	// activity is a ring of hourly request counts for the last day,
	// allocated by the first request served; activityHour is the hour its
	// last slot stands for.
	activity     []uint32
	activityHour int64
	// gc and repack back off after a failed or capacity-skipped maintenance
	// run.
	gc     Backoff
	repack Backoff
}

// Backoff spaces out retries of work that keeps failing. The first failure
// waits base, each further failure doubles the delay up to limit, and any
// success resets it. The zero value is healthy. It takes no lock of its
// own: the mutex of the entry holding it guards the fields.
//
// Due reports whether an attempt may run at now. Wait reports how much
// longer an attempt must hold back, zero when due. Fail records a failure
// at now. Reset clears the history after a success.
type Backoff struct {
	failed time.Time
	delay  time.Duration
}

func (b *Backoff) Due(now time.Time) bool {
	return b.failed.IsZero() || !now.Before(b.failed.Add(b.delay))
}

func (b *Backoff) Wait(now time.Time) time.Duration {
	if b.Due(now) {
		return 0
	}
	return b.failed.Add(b.delay).Sub(now)
}

func (b *Backoff) Fail(now time.Time, base, limit time.Duration) {
	if b.failed.IsZero() {
		b.delay = base
	} else {
		b.delay = min(2*b.delay, limit)
	}
	b.failed = now
}

func (b *Backoff) Reset() { *b = Backoff{} }

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

// hydrationFailureBackoff starts the wait after a failed hydration. A scope
// whose roots cannot be fetched within fetch_timeout would otherwise make
// every request repeat the full attempt under the exclusive gate; the wait
// doubles up to failureBackoffLimit, so a storm on such a repository passes
// through after one attempt instead of one per request.
// maintenanceFailureBackoff starts the wait before a failed GC or repack is
// retried; without it the minute-based scan restarted a timed-out GC on
// every pass. The three are variables so tests can watch them pass.
var (
	hydrationFailureBackoff   = 30 * time.Second
	maintenanceFailureBackoff = time.Minute
	// failureBackoffLimit caps every failure backoff.
	failureBackoffLimit = time.Hour
)

// hydrationBackoffSources bounds the scopes remembered for failed
// hydrations; only a fleet of distinct failing repositories reaches it, and
// a dropped record costs one more attempt.
const hydrationBackoffSources = 4096

// Hydration backoff lives on the store rather than on the scope's entry: a
// scope whose very first hydration fails never publishes bytes, and its
// entry is dropped as empty when the lease is released. Remembered here,
// the failure still spares the next requests the repeat attempt.

// hydrationBlocked reports whether the scope's last hydration failed inside
// the backoff window.
func (s *Store) hydrationBlocked(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	backoff := s.hydrationBackoffs[key]
	return backoff != nil && !backoff.Due(time.Now())
}

// hydrationFailed records a failed hydration of the scope and reports the
// wait it started.
func (s *Store) hydrationFailed(key string) time.Duration {
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.hydrationBackoffs == nil {
		s.hydrationBackoffs = make(map[string]*Backoff)
	}
	backoff := s.hydrationBackoffs[key]
	if backoff == nil {
		if len(s.hydrationBackoffs) >= hydrationBackoffSources {
			for other, entry := range s.hydrationBackoffs {
				if entry.Due(now) {
					delete(s.hydrationBackoffs, other)
				}
			}
			for other := range s.hydrationBackoffs {
				if len(s.hydrationBackoffs) < hydrationBackoffSources {
					break
				}
				delete(s.hydrationBackoffs, other)
			}
		}
		backoff = &Backoff{}
		s.hydrationBackoffs[key] = backoff
	}
	backoff.Fail(now, hydrationFailureBackoff, failureBackoffLimit)
	return backoff.Wait(time.Now())
}

// hydrationWait reports how much longer the scope's hydration is backed off.
func (s *Store) hydrationWait(key string) time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	backoff := s.hydrationBackoffs[key]
	if backoff == nil {
		return 0
	}
	return backoff.Wait(time.Now())
}

// hydrationRecovered forgets a scope's failed hydrations after a success.
func (s *Store) hydrationRecovered(key string) {
	s.mu.Lock()
	delete(s.hydrationBackoffs, key)
	s.mu.Unlock()
}

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
	m, err := measureScope(path)
	return m.size, err
}

// scopeMeasure is the result of one pass over a repository scope directory.
type scopeMeasure struct {
	size         int64 // logical regular-file bytes
	regularPacks int   // object packs without a cruft .mtimes sidecar
	// packFiles names the published response-pack entries, so the writer
	// that measured the scope can drop index entries whose file is gone.
	packFiles map[string]struct{}
}

// measureScope walks a scope once and reports what capacity accounting and
// the maintenance queue need. Links and directories are not counted.
func measureScope(path string) (scopeMeasure, error) {
	var m scopeMeasure
	packCache := filepath.Join(path, "packcache")
	objectPacks := filepath.Join(path, "repo.git", "objects", "pack")
	var packs, cruft map[string]bool
	err := filepath.WalkDir(path, func(p string, d fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if filepath.Dir(p) == packCache {
			// Response-pack bytes are counted through the index, not here.
			if filepath.Ext(p) != ".staging" {
				if m.packFiles == nil {
					m.packFiles = make(map[string]struct{})
				}
				m.packFiles[filepath.Base(p)] = struct{}{}
			}
			return nil
		}
		m.size += info.Size()
		if filepath.Dir(p) == objectPacks {
			switch filepath.Ext(p) {
			case ".pack":
				if packs == nil {
					packs = make(map[string]bool)
				}
				packs[strings.TrimSuffix(p, ".pack")] = true
			case ".mtimes":
				if cruft == nil {
					cruft = make(map[string]bool)
				}
				cruft[strings.TrimSuffix(p, ".mtimes")] = true
			}
		}
		return nil
	})
	for pack := range packs {
		if !cruft[pack] {
			m.regularPacks++
		}
	}
	return m, err
}

// applyMeasureLocked publishes a scope rescan: the measured bytes plus the
// response-pack bytes its index accounts for become the scope size, and the
// difference reaches the store total. An index entry whose file the rescan
// no longer saw leaves the index together with its bytes. Caller holds
// Store.mu.
func (s *Store) applyMeasureLocked(e *entry, m scopeMeasure) {
	for hexed, p := range e.packs {
		if _, present := m.packFiles[hexed+"-"+strconv.FormatInt(p.size, 10)]; !present {
			delete(e.packs, hexed)
			continue
		}
		m.size += p.size
	}
	s.usedBytes += m.size - e.size
	e.size = m.size
	e.regularPacks = m.regularPacks
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
		measured, err := measureScope(path)
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
		e := &entry{key: key, changed: make(chan struct{}), accessed: info.ModTime(), accessFlushed: info.ModTime(), lastGC: lastGC, packs: scanPackIndex(filepath.Join(path, "packcache")), site: s.loadSite(path)}
		s.applyMeasureLocked(e, measured)
		s.entries[key] = e
	}
	return nil
}

// accessFlushInterval bounds persisted recency-stamp writes: within one,
// releases only update the in-memory timestamp. LRU decisions use
// in-memory recency; the stamp only orders cold scopes after a restart.
const accessFlushInterval = 30 * time.Second

// packEntry is one indexed response-pack file: its published size and the
// last time it was served. Recency lives in memory only; after a restart
// it starts from the file's modification time, which is the publish time.
type packEntry struct {
	size     int64
	accessed time.Time
}

// scanPackIndex rebuilds the response-pack index from published entry
// names (<key hex>-<size>). Staging leftovers were swept before this runs.
func scanPackIndex(dir string) map[string]*packEntry {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var index map[string]*packEntry
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
			index = make(map[string]*packEntry)
		}
		published := time.Time{}
		if info, err := entry.Info(); err == nil {
			published = info.ModTime()
		}
		index[name[:dash]] = &packEntry{size: size, accessed: published}
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

// evictScope moves an evicted repository out of the managed tree, so its
// contents can be deleted without the store mutex and a new scope of the
// same key never shares a directory with a deletion in progress. It is
// swapped out by tests to simulate eviction failures.
var evictScope = os.Rename

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
		// Only the rename runs under the mutex: a repository that cannot be
		// moved — a response still streaming on Windows holds its files —
		// is skipped, and the bytes are deleted afterwards off the lock.
		s.evicted++
		trash := filepath.Join(s.Tmp, fmt.Sprintf("evicted-%d-%d", os.Getpid(), s.evicted))
		if err := evictScope(path, trash); err != nil {
			// A busy candidate must not wedge admission for the rest of the
			// cache. Evict the remaining candidates and report the failure
			// only when capacity is still short.
			if evictErr == nil {
				evictErr = fmt.Errorf("evict repository cache: %w", err)
			}
			continue
		}
		s.trash = append(s.trash, trash)
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

// cleanTrash deletes evicted repositories off the store mutex, in the
// background: a request whose admission triggered eviction must not wait
// for another repository's files to disappear. One cleaner at a time drains
// whatever is pending; a directory that cannot be removed yet stays queued
// for the next pass and for the startup sweep.
func (s *Store) cleanTrash() {
	s.mu.Lock()
	if len(s.trash) == 0 || s.cleaning {
		s.mu.Unlock()
		return
	}
	s.cleaning = true
	s.mu.Unlock()
	for {
		s.mu.Lock()
		if len(s.trash) == 0 {
			s.cleaning = false
			s.mu.Unlock()
			return
		}
		pending := s.trash
		s.trash = nil
		s.mu.Unlock()
		for _, path := range pending {
			if err := os.RemoveAll(path); err != nil {
				s.Git.Logger().LogAttrs(context.Background(), slog.LevelDebug, "evicted repository cleanup deferred", logging.ErrorAttrs(err)...)
				s.mu.Lock()
				s.trash = append(s.trash, path)
				s.mu.Unlock()
			}
		}
	}
}

type reservation struct {
	lease *Lease
	bytes int64
}

func (l *Lease) reserve(additional int64) (*reservation, error) {
	s := l.store
	s.mu.Lock()
	additional = max(additional, 0)
	if l.entry.size > s.maxBytes-additional {
		s.mu.Unlock()
		return nil, ErrCapacity
	}
	if err := s.makeRoomLocked(additional); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	s.reservedBytes += additional
	s.mu.Unlock()
	s.scheduleTrashClean()
	return &reservation{lease: l, bytes: additional}, nil
}

// scheduleTrashClean starts the background cleaner when evicted directories
// are waiting and no cleaner is running.
func (s *Store) scheduleTrashClean() {
	s.mu.Lock()
	pending := len(s.trash) > 0 && !s.cleaning
	s.mu.Unlock()
	if pending {
		go s.cleanTrash()
	}
}

func (r *reservation) release() {
	s, e := r.lease.store, r.lease.entry
	// The caller still holds its exclusive gate, so the scope is measured
	// before the store mutex is taken, like a writer's release.
	measured, err := measureScope(filepath.Dir(r.lease.Dir))
	s.mu.Lock()
	s.reservedBytes -= r.bytes
	if err == nil {
		s.applyMeasureLocked(e, measured)
	} else {
		// Keep the admission estimate charged when the filesystem cannot be measured.
		s.usedBytes += r.bytes
		e.size += r.bytes
	}
	s.mu.Unlock()
}
