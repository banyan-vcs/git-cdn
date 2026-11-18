package cache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/nativegit"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testStore(t *testing.T, limit int64) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), nil, limit)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	return s
}

func TestRestartRemovesOrphanedTemporaryFiles(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil, 1024)
	require.NoError(t, err)
	for _, name := range []string{"response-old", "hydrate-old", "view-old"} {
		path := filepath.Join(s.Tmp, name)
		require.NoError(t, os.MkdirAll(path, 0700))
		require.NoError(t, os.WriteFile(filepath.Join(path, "data"), []byte("orphan"), 0600))
	}
	require.NoError(t, s.Close())
	s, err = Open(root, nil, 1024)
	require.NoError(t, err)
	defer s.Close()
	files, err := os.ReadDir(s.Tmp)
	require.NoError(t, err, "orphaned temporary files retained: %v %v", files, err)
	require.Len(t, files, 0, "orphaned temporary files retained: %v %v", files, err)
	{
		info, err := os.Stat(filepath.Join(root, ".scope-key"))
		require.NoError(t, err, "persistent scope key removed")
		require.EqualValues(t, 32, info.Size(), "persistent scope key removed")
	}
}

func testLease(t *testing.T, s *Store, name string) *Lease {
	t.Helper()
	l, err := s.Acquire(context.Background(), s.Key(name, "credential"))
	require.NoError(t, err)
	return l
}

func publishBytes(t *testing.T, l *Lease, size int) error {
	t.Helper()
	require.NoError(t, l.store.ensureScope(l.entry.key))
	stage, err := os.MkdirTemp(l.store.Tmp, "test-stage-")
	require.NoError(t, err)
	defer os.RemoveAll(stage)
	require.NoError(t, os.WriteFile(filepath.Join(stage, "data"), make([]byte, size), 0600))
	return l.publish(context.Background(), stage, nil, false)
}

func storedBytes(t *testing.T, s *Store, name string, size int) string {
	t.Helper()
	l := testLease(t, s, name)
	require.NoError(t, publishBytes(t, l, size))
	path := l.Dir
	l.Release()
	return path
}

func TestLRUEvictsLeastRecentlyUsedWholeScope(t *testing.T) {
	s := testStore(t, 100)
	a := storedBytes(t, s, "a", 40)
	b := storedBytes(t, s, "b", 40)
	l := testLease(t, s, "a")
	l.Release()
	c := storedBytes(t, s, "c", 40)
	{
		_, err := os.Stat(b)
		require.True(t, os.IsNotExist(err), "least recently used scope survived")
	}
	for _, path := range []string{a, c} {
		{
			_, err := os.Stat(path)
			require.NoError(t, err, "recent scope evicted", err)
		}
	}
	stats := s.Stats()
	require.EqualValues(t, 80, stats.Bytes, "wrong accounting: %+v", stats)
	require.EqualValues(t, 1, stats.Evictions, "wrong accounting: %+v", stats)
	require.EqualValues(t, 40, stats.EvictedBytes, "wrong accounting: %+v", stats)
	require.EqualValues(t, 2, stats.Entries, "wrong accounting: %+v", stats)
}

func TestPinnedScopeRejectsAdmissionUntilReleased(t *testing.T) {
	s := testStore(t, 100)
	a := storedBytes(t, s, "a", 60)
	active := testLease(t, s, "a")
	b := testLease(t, s, "b")
	{
		err := publishBytes(t, b, 80)
		require.ErrorIs(t, err, ErrCapacity, "expected capacity failure, got %v", err)
	}
	{
		_, err := os.Stat(a)
		require.NoError(t, err, "active cache was removed", err)
	}
	require.EqualValues(t, 0, s.Stats().ReservedBytes, "failed admission leaked reservation")
	active.Release()
	require.NoError(t, publishBytes(t, b, 80))
	b.Release()
	{
		_, err := os.Stat(a)
		require.True(t, os.IsNotExist(err), "released old scope was not evicted")
	}
}

func TestQueuedLeasePinsTheSameEntry(t *testing.T) {
	s := testStore(t, 100)
	storedBytes(t, s, "a", 60)
	active := testLease(t, s, "a")
	queued := make(chan *Lease, 1)
	go func() {
		l, err := s.Acquire(context.Background(), active.entry.key)
		if err != nil {
			assert.NoError(t, err)
			return
		}
		queued <- l
	}()
	deadline := time.After(2 * time.Second)
	for {
		s.mu.Lock()
		users := active.entry.users
		s.mu.Unlock()
		if users == 2 {
			break
		}
		select {
		case <-deadline:
			require.FailNow(t, "test failed", "waiter was not registered")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	active.Release()
	var next *Lease
	select {
	case next = <-queued:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "test failed", "queued lease did not acquire")
	}
	require.Same(t, active.entry, next.entry, "queued lease got a replacement gate")
	b := testLease(t, s, "b")
	{
		err := publishBytes(t, b, 80)
		require.ErrorIs(t, err, ErrCapacity, "queued entry did not remain pinned")
	}
	next.Release()
	b.Release()
}

func TestReservationsPreventConcurrentOvercommit(t *testing.T) {
	s := testStore(t, 100)
	a, b := testLease(t, s, "a"), testLease(t, s, "b")
	r, err := a.reserve(80)
	require.NoError(t, err)
	{
		_, err := b.reserve(80)
		require.ErrorIs(t, err, ErrCapacity, "concurrent admission exceeded capacity")
	}
	r.release()
	r, err = b.reserve(80)
	require.NoError(t, err, "reservation was not returned", err)
	r.release()
	a.Release()
	b.Release()
	require.EqualValues(t, 0, s.Stats().ReservedBytes, "reservation leaked")
}

func TestOversizedScopeDoesNotEvictUsefulCache(t *testing.T) {
	s := testStore(t, 100)
	a := storedBytes(t, s, "a", 20)
	l := testLease(t, s, "oversized")
	{
		err := publishBytes(t, l, 101)
		require.ErrorIs(t, err, ErrCapacity, "oversized entry admitted")
	}
	l.Release()
	{
		_, err := os.Stat(a)
		require.NoError(t, err, "oversized request evicted useful data")
		require.EqualValues(t, 0, s.Stats().Evictions, "oversized request evicted useful data")
	}
}

func TestLRUPersistsAcrossRestartAndLowerLimit(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil, 100)
	require.NoError(t, err)
	a := storedBytes(t, s, "a", 40)
	b := storedBytes(t, s, "b", 40)
	l := testLease(t, s, "a")
	l.Release()
	require.NoError(t, s.Close())
	s, err = Open(root, nil, 60)
	require.NoError(t, err)
	defer s.Close()
	{
		_, err := os.Stat(a)
		require.NoError(t, err, "recent cache lost on restart")
	}
	{
		_, err := os.Stat(b)
		require.True(t, os.IsNotExist(err), "startup did not trim least recent cache")
	}
	require.EqualValues(t, 40, s.Stats().Bytes, "startup size accounting incorrect")
}

func TestManagedDirectorySymlinkIsRejected(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	marker := filepath.Join(outside, "keep")
	require.NoError(t, os.WriteFile(marker, []byte("keep"), 0600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "repos")))
	if s, err := Open(root, nil, 1); err == nil {
		s.Close()
		require.FailNow(t, "test failed", "managed root symlink was followed")
	}
	{
		_, err := os.Stat(marker)
		require.NoError(t, err, "outside data was modified")
	}
}

func TestConcurrentScopePublicationStaysWithinLimit(t *testing.T) {
	s := testStore(t, 100)
	var wg sync.WaitGroup
	for _, name := range []string{"a", "b", "c", "d", "e", "f"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			l := testLease(t, s, name)
			err := publishBytes(t, l, 40)
			l.Release()
			assert.False(t, err != nil && !errors.Is(err, ErrCapacity))
		}(name)
	}
	wg.Wait()
	size, err := directorySize(filepath.Join(s.Root, "repos"))
	require.NoError(t, err)
	require.False(t, size > 100, "concurrent accounting exceeded limit: size=%d stats=%+v", size, s.Stats())
	require.EqualValues(t, size, s.Stats().Bytes, "concurrent accounting exceeded limit: size=%d stats=%+v", size, s.Stats())
	require.EqualValues(t, 0, s.Stats().ReservedBytes, "concurrent accounting exceeded limit: size=%d stats=%+v", size, s.Stats())
}

func TestGrowingStagingRepositoryIsCancelled(t *testing.T) {
	s := testStore(t, 2048)
	// Use a helper process so shell/tool startup does not dominate the cancellation deadline.
	runner, err := nativegit.New(os.Args[0], 1)
	require.NoError(t, err)
	s.Git = runner
	l := testLease(t, s, "large")
	defer l.Release()
	stage := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	started := time.Now()
	{
		err := l.fetchBounded(ctx, stage, map[string]string{"GIT_CDN_TEST_STAGING_GROWTH": "1"}, "-test.run=^TestStagingGrowthHelperProcess$")
		require.ErrorIs(t, err, ErrCapacity, "staging growth not cancelled: %v", err)
	}
	require.False(t, time.Since(started) >= 2*time.Second, "staging capacity cancellation was delayed")
}

func TestStagingGrowthHelperProcess(t *testing.T) {
	if os.Getenv("GIT_CDN_TEST_STAGING_GROWTH") != "1" {
		t.Skip("subprocess fixture")
	}
	require.NoError(t, os.MkdirAll("objects/pack", 0700))
	require.NoError(t, os.WriteFile("objects/pack/growing.tmp", make([]byte, 4096), 0600))
	time.Sleep(30 * time.Second)
}
