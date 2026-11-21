package cache

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The response pack cache stores complete upload-pack responses verbatim,
// keyed by the serving side (repository identity, exact request bytes and
// the live ref snapshot). Entries are self-contained: a hit can stream the
// stored file without consulting repository objects. Correctness of reuse
// relies on the request pipeline: the snapshot fingerprint covers ref views
// (new tags change include-tag output even for identical want sets), and
// want OIDs are validated against the live snapshot before prepare runs.
// Entries share the repository scope, so they count toward the persistent
// cache budget and disappear with whole-scope LRU eviction.

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
// after its directory disappears.
func (s *Store) LookupPack(scope string, key []byte) (*os.File, error) {
	if s.PackCacheBytes() <= 0 || len(key) != 32 || !validKey(scope) {
		return nil, nil
	}
	dir := s.packDir(scope)
	prefix := fmt.Sprintf("%x-", key)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil // unreadable entries are treated as absent
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasSuffix(name, ".staging") || !strings.HasPrefix(name, prefix) {
			continue
		}
		size, err := strconv.ParseInt(strings.TrimPrefix(name, prefix), 10, 64)
		if err != nil || size <= 0 {
			continue
		}
		path := filepath.Join(dir, name)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.Size() != size {
			_ = os.Remove(path)
			return nil, nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil, nil
		}
		return file, nil
	}
	return nil, nil
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
// the repository's response-pack budget oldest-first. It reports whether a
// new entry was written; failures are recoverable, the caller keeps serving
// from the spool. Concurrent shared leases serialize pack publication
// through the scope's pack mutex without excluding readers elsewhere.
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
	l.prunePacks(dir, limit)
	return true, nil
}

// prunePacks removes stray staging files and the oldest entries until the
// directory fits the budget. The lease guarantees exclusive access.
func (l *Lease) prunePacks(dir string, limit int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	type packed struct {
		path string
		size int64
	}
	var total int64
	var packs []packed
	for _, entry := range entries {
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if filepath.Ext(entry.Name()) == ".staging" {
			_ = os.Remove(path)
			continue
		}
		packs = append(packs, packed{path: path, size: info.Size()})
		total += info.Size()
	}
	sort.Slice(packs, func(i, j int) bool {
		a, errA := os.Stat(packs[i].path)
		b, errB := os.Stat(packs[j].path)
		if errA != nil || errB != nil {
			return packs[i].path < packs[j].path
		}
		return a.ModTime().Before(b.ModTime())
	})
	for _, p := range packs {
		if total <= limit {
			break
		}
		if err := os.Remove(p.path); err == nil {
			total -= p.size
		}
	}
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
