package cache

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
)

// The response pack cache stores complete upload-pack responses verbatim,
// keyed by the serving side (repository identity, request content and the
// live ref snapshot). Entries are self-contained: a hit can stream the
// stored file without consulting repository objects. Correctness of reuse
// relies on the request pipeline: the snapshot fingerprint covers ref views
// (new tags change include-tag output even for identical want sets), and
// want OIDs are validated against the live snapshot before prepare runs.
// Entries share the repository scope, so they count toward the persistent
// cache budget and disappear with whole-scope LRU eviction.
//
// The index is the only authority for how many bytes the entries occupy:
// directory rescans leave files under packcache/ out of the scope size and
// the index sizes are added instead. Storing, pruning, invalidating and
// losing a file then each adjust the budget exactly once, whichever order
// they happen in.

func (s *Store) packDir(scope string) string {
	return filepath.Join(s.Root, "repos", scope, "packcache")
}

func (l *Lease) packDir() string {
	return l.store.packDir(l.entry.key)
}

// Entries carry the published size in their name and a mismatch on lookup
// (truncation, tampering) removes the entry and forces regeneration.
func packName(key []byte, size int64) string {
	return fmt.Sprintf("%x-%d", key, size)
}

// LookupPack opens the stored response for key in the named scope. A nil
// file with a nil error means the key is absent; the caller streams the
// file and closes it. No repository lease is required: racing eviction is
// safe because callers pin the scope, and an opened file streams even
// after its directory disappears. The in-memory index turns the hot path
// into one Stat; a mismatch (truncation, tampering, external removal)
// drops the index entry and the file so the next request regenerates.
func (s *Store) LookupPack(scope string, key []byte) (*os.File, error) {
	if s.PackCacheBytes() <= 0 || len(key) != 32 || !validKey(scope) {
		return nil, nil
	}
	hexed := fmt.Sprintf("%x", key)
	s.mu.Lock()
	e := s.entries[scope]
	if e == nil || e.packs == nil {
		s.mu.Unlock()
		return nil, nil
	}
	p := e.packs[hexed]
	if p != nil {
		// Recency is in-memory only: it orders pruning inside this process.
		p.accessed = time.Now()
	}
	s.mu.Unlock()
	if p == nil {
		return nil, nil
	}
	dir := s.packDir(scope)
	path := filepath.Join(dir, packName(key, p.size))
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			s.forgetPack(scope, hexed)
			s.Git.Logger().Debug("response pack cache entry missing", "cache_key", scope)
		} else {
			logging.Failure(context.Background(), s.Git.Logger(), "response pack cache lookup failed", err, slog.String("cache_key", scope))
		}
		return nil, nil
	}
	if info.Size() != p.size {
		s.Git.Logger().Warn("response pack cache entry invalidated", "cache_key", scope, "reason", "size_mismatch", "expected_bytes", p.size, "actual_bytes", info.Size())
		_ = os.Remove(path)
		s.forgetPack(scope, hexed)
		return nil, nil
	}
	file, err := os.Open(path)
	if err != nil {
		logging.Failure(context.Background(), s.Git.Logger(), "response pack cache lookup failed", err, slog.String("cache_key", scope))
		return nil, nil
	}
	return file, nil
}

// forgetPack drops an index entry whose file no longer matches and returns
// its bytes to the budget. Caller has released mu; the map write re-acquires it.
func (s *Store) forgetPack(scope, hexed string) {
	s.mu.Lock()
	if e := s.entries[scope]; e != nil && e.packs != nil {
		if p := e.packs[hexed]; p != nil {
			delete(e.packs, hexed)
			e.size -= p.size
			s.usedBytes -= p.size
		}
	}
	s.mu.Unlock()
}

// LookupPack opens a stored response for key under the lease's scope.
func (l *Lease) LookupPack(key []byte) (*os.File, error) {
	return l.store.LookupPack(l.entry.key, key)
}

// sweepStaging removes response-pack staging leftovers from interrupted
// publications. The directory lock excludes live requests at startup, so
// any staging file present is an orphan; published entries never carry the
// staging suffix.
func sweepStaging(dir string) {
	files, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, file := range files {
		if !file.IsDir() && strings.HasSuffix(file.Name(), ".staging") {
			_ = os.Remove(filepath.Join(dir, file.Name()))
		}
	}
}

// StorePack publishes the spooled response at source under key and prunes
// the repository's response-pack budget. It reports whether a new entry
// was written; failures are recoverable, the caller keeps serving from the
// spool. Concurrent shared leases serialize pack publication through the
// scope's pack mutex without excluding readers elsewhere. The entry's bytes
// enter the scope size and the store budget here, whichever lease stored
// them: a reader lease never rescans its scope, so without this the budget
// would miss every entry stored on the shared path.
func (l *Lease) StorePack(key []byte, source string) (bool, error) {
	l.entry.packMu.Lock()
	defer l.entry.packMu.Unlock()
	limit := l.store.PackCacheBytes()
	if limit <= 0 || len(key) != 32 {
		return false, nil
	}
	dir := l.packDir()
	if err := ensureDirectory(dir); err != nil {
		return false, err
	}
	staging := filepath.Join(dir, fmt.Sprintf("%x", key)) + ".staging"
	_ = os.Remove(staging)
	if err := linkOrCopy(source, staging); err != nil {
		return false, err
	}
	info, err := os.Lstat(staging)
	if err != nil {
		return false, err
	}
	if info.Size() > limit {
		_ = os.Remove(staging)
		return false, nil // a response this large never fits the budget
	}
	target := filepath.Join(dir, packName(key, info.Size()))
	if _, err := os.Lstat(target); err == nil {
		_ = os.Remove(staging)
		return false, nil
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Remove(staging)
		return false, err
	}
	hexed := fmt.Sprintf("%x", key)
	// Drop a stale entry from an earlier generation with the same key and
	// a different size, so the index and the directory keep exactly one
	// file per response key.
	if published, err := os.ReadDir(dir); err == nil {
		prefix := hexed + "-"
		for _, stale := range published {
			name := stale.Name()
			if name != filepath.Base(target) && strings.HasPrefix(name, prefix) {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
	now := time.Now()
	l.store.mu.Lock()
	if l.entry.packs == nil {
		l.entry.packs = make(map[string]*packEntry)
	}
	// The index entry being replaced accounts for the stale generation's
	// bytes; the new entry is charged in its place.
	var previous int64
	if old := l.entry.packs[hexed]; old != nil {
		previous = old.size
	}
	l.entry.packs[hexed] = &packEntry{size: info.Size(), accessed: now}
	l.entry.size += info.Size() - previous
	l.store.usedBytes += info.Size() - previous
	l.store.mu.Unlock()
	l.prunePacks(dir, limit)
	// Like a writer's release, a stored entry may push the store over its
	// budget; the oldest idle scopes make room.
	l.store.mu.Lock()
	_ = l.store.makeRoomLocked(0)
	l.store.mu.Unlock()
	l.store.scheduleTrashClean()
	return true, nil
}

// prunePacks removes stray staging files and the least recently served
// entries until the directory fits the budget. The lease guarantees no
// other publisher touches the directory meanwhile.
func (l *Lease) prunePacks(dir string, limit int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type packed struct {
		path, hexed string
		size        int64
		accessed    time.Time
		indexed     bool
	}
	var total int64
	var packs []packed
	var staging []string
	l.store.mu.Lock()
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) == ".staging" {
			staging = append(staging, path)
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		p := packed{path: path, size: info.Size(), accessed: info.ModTime()}
		if dash := strings.LastIndexByte(entry.Name(), '-'); dash > 0 {
			p.hexed = entry.Name()[:dash]
			if indexed := l.entry.packs[p.hexed]; indexed != nil {
				p.accessed, p.indexed = indexed.accessed, true
			}
		}
		packs = append(packs, p)
		total += info.Size()
	}
	// Oldest recency first. An entry outside the index was never served by
	// this process and is not reachable through it; it goes before any
	// indexed entry, oldest publish first.
	sort.Slice(packs, func(i, j int) bool {
		a, b := packs[i], packs[j]
		if a.indexed != b.indexed {
			return !a.indexed
		}
		if a.accessed.Equal(b.accessed) {
			return a.path < b.path
		}
		return a.accessed.Before(b.accessed)
	})
	l.store.mu.Unlock()
	for _, path := range staging {
		_ = os.Remove(path)
	}
	var evicted, evictedBytes int64
	var removed []packed
	for _, p := range packs {
		if total <= limit {
			break
		}
		if err := os.Remove(p.path); err == nil {
			total -= p.size
			evicted++
			evictedBytes += p.size
			removed = append(removed, p)
		}
	}
	if evicted == 0 {
		return
	}
	l.store.mu.Lock()
	for _, p := range removed {
		if indexed := l.entry.packs[p.hexed]; indexed != nil && indexed.size == p.size {
			delete(l.entry.packs, p.hexed)
			l.entry.size -= p.size
			l.store.usedBytes -= p.size
		}
	}
	l.store.mu.Unlock()
	l.store.Git.Logger().Info("response pack cache pruned", "cache_key", l.entry.key, "entries_evicted", evicted,
		"evicted_bytes", evictedBytes, "remaining_bytes", total, "max_pack_cache_bytes", limit)
}

func linkOrCopy(source, target string) error {
	if err := os.Link(source, target); err == nil {
		return nil
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	return err
}
