package cache

import (
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
	order := staleEntry.accessed.After(freshEntry.accessed)
	dirty := staleEntry.accessDirty
	s.mu.Unlock()
	require.True(t, order, "in-memory recency did not advance")
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

func hexed(key []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 0, len(key)*2)
	for _, b := range key {
		out = append(out, digits[b>>4], digits[b&0xf])
	}
	return string(out)
}
