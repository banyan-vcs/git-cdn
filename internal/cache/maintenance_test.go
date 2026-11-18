package cache

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/nativegit"
	"github.com/chiyutianyi/git-cdn/internal/protocol"
	"github.com/stretchr/testify/require"
)

func maintenanceGit(t *testing.T, g *nativegit.Runner, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if len(args) > 0 && args[0] == "commit" {
		require.NoError(t, g.Run(ctx, dir, nil, os.Stderr, map[string]string{"GIT_AUTHOR_NAME": "Cache Test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "Cache Test", "GIT_COMMITTER_EMAIL": "test@example.invalid"}, args...))
		return ""
	}
	output, err := g.Output(ctx, dir, "", args...)
	require.NoError(t, err, "git %v", args)
	return strings.TrimSpace(string(output))
}

func populatedLease(t *testing.T, threshold int) (*Store, *Lease, []string) {
	t.Helper()
	g, err := nativegit.New("git", 4)
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 64<<20, RepackOptions{PackThreshold: threshold, Timeout: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	l, err := s.Acquire(context.Background(), s.Key("origin/repository.git:sha1"))
	require.NoError(t, err)
	work := filepath.Join(t.TempDir(), "work")
	maintenanceGit(t, g, "", "init", "-q", "--object-format=sha1", "--initial-branch=main", work)
	var roots []string
	for i := 0; i < 10; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(work, "data"), []byte(fmt.Sprintf("revision %d\n", i)), 0600))
		maintenanceGit(t, g, work, "add", "data")
		maintenanceGit(t, g, work, "commit", "-q", "-m", fmt.Sprintf("revision %d", i))
		root := maintenanceGit(t, g, work, "rev-parse", "HEAD")
		hit, err := l.Ensure(context.Background(), (&url.URL{Scheme: "file", Path: work}).String(), nil, []string{root})
		require.NoError(t, err)
		require.False(t, hit)
		roots = append(roots, root)
	}
	require.GreaterOrEqual(t, packCount(l.Dir), 2)
	return s, l, roots
}

func TestGeometricRepackPreservesEveryCachedRoot(t *testing.T) {
	s, l, roots := populatedLease(t, 4)
	defer l.Release()
	before := packCount(l.Dir)
	changed, err := l.Repack(context.Background())
	require.NoError(t, err)
	require.True(t, changed)
	require.Less(t, packCount(l.Dir), before)
	for _, root := range roots {
		require.True(t, l.complete(context.Background(), []string{root}, false))
	}
	maintenanceGit(t, s.Git, l.Dir, "fsck", "--connectivity-only", "--no-dangling")
	require.Zero(t, s.Stats().ReservedBytes)
}

func TestRepackBudgetFailureLeavesOriginalPacksReadable(t *testing.T) {
	s, l, roots := populatedLease(t, 4)
	defer l.Release()
	before, err := os.ReadDir(filepath.Join(l.Dir, "objects", "pack"))
	require.NoError(t, err)
	s.mu.Lock()
	s.maxBytes = l.entry.size + 1
	s.mu.Unlock()
	changed, err := l.Repack(context.Background())
	require.ErrorIs(t, err, ErrCapacity)
	require.False(t, changed)
	after, err := os.ReadDir(filepath.Join(l.Dir, "objects", "pack"))
	require.NoError(t, err)
	var oldNames, newNames []string
	for _, file := range before {
		oldNames = append(oldNames, file.Name())
	}
	for _, file := range after {
		newNames = append(newNames, file.Name())
	}
	require.Equal(t, oldNames, newNames)
	require.True(t, l.complete(context.Background(), roots, false))
	require.Zero(t, s.Stats().ReservedBytes)
	files, err := os.ReadDir(s.Tmp)
	require.NoError(t, err)
	require.Empty(t, files)
}

func TestBackgroundRepackWaitsForQueuedRepositoryLease(t *testing.T) {
	s, l, roots := populatedLease(t, 4)
	before := packCount(l.Dir)
	acquired := make(chan *Lease, 1)
	go func() {
		next, err := s.Acquire(context.Background(), l.entry.key)
		if err == nil {
			acquired <- next
		}
	}()
	require.Eventually(t, func() bool { s.mu.Lock(); defer s.mu.Unlock(); return l.entry.users == 2 }, time.Second, 5*time.Millisecond)
	l.Release()
	var next *Lease
	select {
	case next = <-acquired:
	case <-time.After(time.Second):
		require.FailNow(t, "queued lease did not acquire")
	}
	require.Zero(t, s.Stats().Repacks)
	require.Equal(t, before, packCount(next.Dir))
	next.Release()
	require.Eventually(t, func() bool { return s.Stats().Repacks > 0 }, 10*time.Second, 10*time.Millisecond)
	lease, err := s.Acquire(context.Background(), s.Key("origin/repository.git:sha1"))
	require.NoError(t, err)
	defer lease.Release()
	require.Less(t, packCount(lease.Dir), before)
	require.True(t, lease.complete(context.Background(), roots, false))
}

func TestLegacyCredentialCachesMergeWithoutRefetch(t *testing.T) {
	g, err := nativegit.New("git", 4)
	require.NoError(t, err)
	root, work := t.TempDir(), filepath.Join(t.TempDir(), "work")
	maintenanceGit(t, g, "", "init", "-q", "--initial-branch=main", "--object-format=sha1", work)
	identity := new(Store).Key("origin/repository.git:sha1")
	var roots []string
	for i, credential := range []string{strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		require.NoError(t, os.WriteFile(filepath.Join(work, "data"), []byte(fmt.Sprintf("revision %d", i)), 0600))
		maintenanceGit(t, g, work, "add", "data")
		maintenanceGit(t, g, work, "commit", "-q", "-m", fmt.Sprintf("revision %d", i))
		oid := maintenanceGit(t, g, work, "rev-parse", "HEAD")
		roots = append(roots, oid)
		legacy := filepath.Join(root, "repos", identity, credential, "repo.git")
		maintenanceGit(t, g, "", "init", "-q", "--bare", "--object-format=sha1", legacy)
		maintenanceGit(t, g, legacy, "-c", "fetch.unpackLimit=0", "fetch", "--no-auto-maintenance", (&url.URL{Scheme: "file", Path: work}).String(), oid+":refs/cache/roots/"+oid)
		require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(legacy), ".access"), nil, 0600))
	}
	require.NoError(t, os.WriteFile(filepath.Join(root, ".scope-key"), make([]byte, 32), 0600))
	s, err := Open(root, g, 64<<20)
	require.NoError(t, err)
	defer s.Close()
	l, err := s.Acquire(context.Background(), identity)
	require.NoError(t, err)
	defer l.Release()
	require.Equal(t, filepath.Join(root, "repos", identity, "repo.git"), l.Dir)
	for _, oid := range roots {
		hit, err := l.Ensure(context.Background(), "file:///unavailable", nil, []string{oid})
		require.NoError(t, err)
		require.True(t, hit)
	}
	children, err := os.ReadDir(filepath.Dir(l.Dir))
	require.NoError(t, err)
	for _, child := range children {
		require.False(t, digest(child.Name()))
	}
	_, err = os.Stat(filepath.Join(root, ".scope-key"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, 1, s.Stats().Entries)
}

func TestOlderQueuedSnapshotDoesNotRegressSynchronizedRefs(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	observed := time.Now()
	newer := protocol.Snapshot{ObservedAt: observed, Refs: []protocol.Ref{{Name: "HEAD", OID: roots[len(roots)-1], Symref: "refs/heads/main"}, {Name: "refs/heads/main", OID: roots[len(roots)-1]}}}
	require.NoError(t, l.SyncRefs(context.Background(), newer))
	older := protocol.Snapshot{ObservedAt: observed.Add(-time.Second), Refs: []protocol.Ref{{Name: "refs/heads/removed", OID: roots[0]}}}
	require.NoError(t, l.SyncRefs(context.Background(), older))
	require.Equal(t, roots[len(roots)-1], maintenanceGit(t, s.Git, l.Dir, "rev-parse", "refs/cache/upstream/heads/main"))
	_, err := s.Git.Output(context.Background(), l.Dir, "", "show-ref", "--verify", "refs/cache/upstream/heads/removed")
	require.Error(t, err)
}
