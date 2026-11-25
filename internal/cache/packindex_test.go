package cache

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// storeTestPack publishes a source file as a response-pack entry under the
// lease's scope and returns the entry's key bytes.
func storeTestPack(t *testing.T, l *Lease, payload []byte) []byte {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, payload, 0600))
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	stored, err := l.StorePack(key, source)
	require.NoError(t, err)
	require.True(t, stored)
	return key
}

// The response-pack index is rebuilt from published entry names at startup,
// so warm lookups after a restart resolve with one Stat instead of a
// directory scan.
func TestPackIndexSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil, 1<<20)
	require.NoError(t, err)
	s.SetPackCacheBytes(1 << 20)
	l := testLease(t, s, "indexed")
	payload := []byte("response body for the index test")
	key := storeTestPack(t, l, payload)
	l.Release()
	require.NoError(t, s.Close())

	s, err = Open(root, nil, 1<<20)
	require.NoError(t, err)
	s.SetPackCacheBytes(1 << 20)
	defer s.Close()
	scope := s.Key("indexed")
	file, err := s.LookupPack(scope, key)
	require.NoError(t, err)
	require.NotNil(t, file, "rebuilt index lost the entry")
	got, err := os.ReadFile(file.Name())
	require.NoError(t, err)
	file.Close()
	require.Equal(t, payload, got)
}

// A truncated or tampered entry fails its Stat validation once, drops from
// the index and the directory, and the next lookup regenerates.
func TestPackIndexSelfHealsTruncation(t *testing.T) {
	s := testStore(t, 1<<20)
	s.SetPackCacheBytes(1 << 20)
	l := testLease(t, s, "healing")
	key := storeTestPack(t, l, []byte("full response body that will be truncated"))
	scope := l.entry.key
	path := filepath.Join(s.packDir(scope), packName(key, int64(len("full response body that will be truncated"))))
	l.Release()

	require.NoError(t, os.Truncate(path, 4))
	file, err := s.LookupPack(scope, key)
	require.NoError(t, err)
	require.Nil(t, file, "truncated entry still served")
	_, err = os.Stat(path)
	require.True(t, os.IsNotExist(err), "truncated entry survived on disk")
	s.mu.Lock()
	_, indexed := s.entries[scope].packs[string(hexed(key))]
	s.mu.Unlock()
	require.False(t, indexed, "truncated entry stayed in the index")
	file, err = s.LookupPack(scope, key)
	require.NoError(t, err)
	require.Nil(t, file, "self-healed lookup served a stale entry")
}

// Publishing a new generation removes a stale same-key file with a
// different size, so exactly one file per response key remains.
func TestPackStoreRemovesStaleSameKeyEntries(t *testing.T) {
	s := testStore(t, 1<<20)
	s.SetPackCacheBytes(1 << 20)
	l := testLease(t, s, "stale")
	key := storeTestPack(t, l, []byte("first generation body"))
	scope := l.entry.key
	stale := storeTestPack(t, l, []byte("second generation body with a different length"))
	_ = stale
	l.Release()
	entries, err := os.ReadDir(s.packDir(scope))
	require.NoError(t, err)
	require.Len(t, entries, 1, "stale same-key entry survived the new publication")
	file, err := s.LookupPack(scope, key)
	require.NoError(t, err)
	require.NotNil(t, file)
	info, err := file.Stat()
	require.NoError(t, err)
	file.Close()
	require.EqualValues(t, int64(len("second generation body with a different length")), info.Size())
}

// Recency stamps throttle to one persisted write per interval, keep the
// in-memory timestamp fresh, and flush the final ordering at Close so a
// clean restart still evicts the least recently used scope.
func TestAccessStampsThrottleAndFlushAtClose(t *testing.T) {
	s := testStore(t, 1<<20)
	s.SetPackCacheBytes(1 << 20)
	a := storedBytes(t, s, "stale", 64)
	b := storedBytes(t, s, "fresh", 64)
	stamp := func(dir string) time.Time {
		info, err := os.Stat(filepath.Join(dir, ".access"))
		require.NoError(t, err)
		return info.ModTime()
	}
	beforeA, beforeB := stamp(filepath.Dir(a)), stamp(filepath.Dir(b))
	// Touch the stale scope again within the flush interval: the stamp on
	// disk must not move, while in-memory recency and the dirty flag do.
	l := testLease(t, s, "stale")
	l.Release()
	require.Equal(t, beforeA, stamp(filepath.Dir(a)), "throttled release persisted a recency stamp")
	require.Equal(t, beforeB, stamp(filepath.Dir(b)))
	s.mu.Lock()
	staleEntry, freshEntry := s.entries[s.Key("stale")], s.entries[s.Key("fresh")]
	// Coarse clocks can stamp both scopes in the same tick; recency must
	// never fall behind, and the dirty flag is the deterministic signal.
	order := !staleEntry.accessed.Before(freshEntry.accessed)
	dirty := staleEntry.accessDirty
	s.mu.Unlock()
	require.True(t, order, "in-memory recency fell behind the untouched scope")
	require.True(t, dirty, "suppressed flush did not mark the entry dirty")

	require.NoError(t, s.Close())
	s.mu.Lock()
	dirty = staleEntry.accessDirty
	s.mu.Unlock()
	require.False(t, dirty, "Close did not flush the dirty stamp")
	afterA, afterB := stamp(filepath.Dir(a)), stamp(filepath.Dir(b))
	require.False(t, afterA.Before(beforeA), "Close moved the stamp backwards")
	require.Equal(t, beforeB, afterB, "Close flushed an unmodified scope")
}

// storeKeyedPack publishes payload under a key that is unique to id.
func storeKeyedPack(t *testing.T, l *Lease, id byte, payload string) []byte {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	require.NoError(t, os.WriteFile(source, []byte(payload), 0600))
	key := make([]byte, 32)
	key[31] = id
	stored, err := l.StorePack(key, source)
	require.NoError(t, err)
	require.True(t, stored)
	return key
}

// An entry stored under a shared reader lease enters the scope size and the
// store budget at once — a reader never rescans its scope — and pruning,
// a writer's rescan and an external removal each settle the bytes exactly
// once.
func TestStoredPacksEnterTheBudgetOnce(t *testing.T) {
	s := testStore(t, 1<<20)
	s.SetPackCacheBytes(1 << 20)
	writer := testLease(t, s, "accounted")
	scope := writer.entry.key
	writer.Release()
	base := s.Stats().Bytes
	first := []byte("stored on the shared path")

	shared, err := s.AcquireShared(context.Background(), scope)
	require.NoError(t, err)
	storeKeyedPack(t, shared, 1, string(first))
	shared.Release()
	require.EqualValues(t, base+int64(len(first)), s.Stats().Bytes, "a shared-path entry was not charged")

	// A smaller per-scope budget prunes the older entry and returns its bytes.
	s.SetPackCacheBytes(int64(len(first) + 3))
	shared, err = s.AcquireShared(context.Background(), scope)
	require.NoError(t, err)
	secondKey := storeKeyedPack(t, shared, 2, "second")
	shared.Release()
	require.EqualValues(t, base+6, s.Stats().Bytes, "pruning did not return the evicted bytes")

	// A writer's rescan keeps the remaining entry and does not count it twice.
	exclusive := testLease(t, s, "accounted")
	exclusive.Release()
	require.EqualValues(t, base+6, s.Stats().Bytes, "a rescan disagreed with the index")

	// An entry deleted from outside leaves the index and the budget on its
	// next lookup, and a further rescan does not subtract it again.
	entries, err := os.ReadDir(s.packDir(scope))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.NoError(t, os.Remove(filepath.Join(s.packDir(scope), entries[0].Name())))
	file, err := s.LookupPack(scope, secondKey)
	require.NoError(t, err)
	require.Nil(t, file)
	require.EqualValues(t, base, s.Stats().Bytes, "a lost entry kept its bytes charged")
	exclusive = testLease(t, s, "accounted")
	exclusive.Release()
	require.EqualValues(t, base, s.Stats().Bytes, "a rescan charged or refunded a lost entry twice")
}

// Within a scope the least recently served entry is pruned first, not the
// oldest one: a long-lived entry that keeps being served outlives a newer
// one nobody asked for.
func TestPackPruningEvictsLeastRecentlyServed(t *testing.T) {
	s := testStore(t, 1<<20)
	s.SetPackCacheBytes(1 << 20)
	l := testLease(t, s, "recency")
	scope := l.entry.key
	first := storeKeyedPack(t, l, 1, "first response body")
	second := storeKeyedPack(t, l, 2, "second response body")
	// Past the clock's resolution, so the touch is strictly later.
	time.Sleep(20 * time.Millisecond)
	file, err := s.LookupPack(scope, first)
	require.NoError(t, err)
	require.NotNil(t, file)
	file.Close()
	s.SetPackCacheBytes(int64(len("first response body") + len("third body")))
	third := storeKeyedPack(t, l, 3, "third body")
	l.Release()
	entries, err := os.ReadDir(s.packDir(scope))
	require.NoError(t, err)
	require.Len(t, entries, 2, "pruning did not bring the directory within its budget")
	file, err = s.LookupPack(scope, second)
	require.NoError(t, err)
	require.Nil(t, file, "the least recently served entry survived")
	for _, key := range [][]byte{first, third} {
		file, err = s.LookupPack(scope, key)
		require.NoError(t, err)
		require.NotNil(t, file, "a recently served entry was pruned")
		file.Close()
	}
}

func hexed(key []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(key)*2)
	for _, b := range key {
		out = append(out, digits[b>>4], digits[b&0xf])
	}
	return string(out)
}
