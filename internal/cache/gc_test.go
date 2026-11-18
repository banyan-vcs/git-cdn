package cache

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/protocol"
	"github.com/stretchr/testify/require"
)

func orphanBlob(t *testing.T, l *Lease, data string, modified time.Time) string {
	t.Helper()
	out, err := l.store.Git.Output(context.Background(), l.Dir, data, "hash-object", "-w", "--stdin")
	require.NoError(t, err)
	oid := strings.TrimSpace(string(out))
	path := filepath.Join(l.Dir, "objects", oid[:2], oid[2:])
	require.NoError(t, os.Chtimes(path, modified, modified))
	return oid
}

func requireObject(t *testing.T, l *Lease, oid string, present bool) {
	t.Helper()
	_, err := l.store.Git.Output(context.Background(), l.Dir, "", "cat-file", "-e", oid)
	if present {
		require.NoError(t, err, oid)
	} else {
		require.Error(t, err, oid)
	}
}

func TestGCExpiresOldObjectsAndPreservesRecentCruftAcrossRepack(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	now := time.Now()
	old := orphanBlob(t, l, "expired garbage", now.Add(-30*24*time.Hour))
	recent := orphanBlob(t, l, "recent garbage", now.Add(-10*24*time.Hour))
	changed, err := l.rewrite(context.Background(), true, now)
	require.NoError(t, err)
	require.True(t, changed)
	requireObject(t, l, old, false)
	requireObject(t, l, recent, true)
	mtimes, err := filepath.Glob(filepath.Join(l.Dir, "objects", "pack", "*.mtimes"))
	require.NoError(t, err)
	require.NotEmpty(t, mtimes)
	before, err := os.ReadFile(mtimes[0])
	require.NoError(t, err)
	// Add two normal packs so geometric maintenance runs next to the cruft pack.
	for _, root := range []string{roots[0], roots[4]} {
		var output bytes.Buffer
		require.NoError(t, s.Git.Run(context.Background(), l.Dir, strings.NewReader(root+"\n"), &output, nil, "pack-objects", filepath.Join(l.Dir, "objects", "pack", "pack")))
	}
	s.repack.PackThreshold = 2
	_, err = l.Repack(context.Background())
	require.NoError(t, err)
	after, err := os.ReadFile(mtimes[0])
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = l.rewrite(context.Background(), true, now.Add(6*24*time.Hour))
	require.NoError(t, err)
	requireObject(t, l, recent, false)
	require.True(t, l.complete(context.Background(), roots, false))
	require.Zero(t, s.Stats().ReservedBytes)
}

func TestRemovedRootHistoryGetsFourteenDayGrace(t *testing.T) {
	s, l, oldRoots := populatedLease(t, 32)
	defer l.Release()
	now := time.Now()
	packs, err := filepath.Glob(filepath.Join(l.Dir, "objects", "pack", "*.pack"))
	require.NoError(t, err)
	for _, pack := range packs {
		old := now.Add(-60 * 24 * time.Hour)
		require.NoError(t, os.Chtimes(pack, old, old))
	}
	tree := maintenanceGit(t, s.Git, l.Dir, "mktree")
	var output bytes.Buffer
	require.NoError(t, s.Git.Run(context.Background(), l.Dir, strings.NewReader("replacement\n"), &output, map[string]string{"GIT_AUTHOR_NAME": "Cache Test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "Cache Test", "GIT_COMMITTER_EMAIL": "test@example.invalid"}, "commit-tree", tree))
	root := strings.TrimSpace(output.String())
	require.NoError(t, l.SyncRefs(context.Background(), protocol.Snapshot{Refs: []protocol.Ref{{Name: "HEAD", OID: root, Symref: "refs/heads/main"}, {Name: "refs/heads/main", OID: root}}}))
	_, err = l.rewrite(context.Background(), true, now.Add(13*24*time.Hour))
	require.NoError(t, err)
	requireObject(t, l, oldRoots[len(oldRoots)-1], true)
	_, err = l.rewrite(context.Background(), true, now.Add(15*24*time.Hour))
	require.NoError(t, err)
	requireObject(t, l, oldRoots[len(oldRoots)-1], false)
	requireObject(t, l, root, true)
	retained, err := readRetention(l.Dir)
	require.NoError(t, err)
	require.Empty(t, retained)
}

func TestGCWaitsForQueuedLeaseAndRunsBelowRepackThreshold(t *testing.T) {
	s, l, _ := populatedLease(t, 32)
	old := orphanBlob(t, l, "expired garbage", time.Now().Add(-30*24*time.Hour))
	s.mu.Lock()
	l.entry.lastGC = time.Now().Add(-2 * s.repack.GCInterval)
	s.mu.Unlock()
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
	require.Zero(t, s.Stats().GCs)
	requireObject(t, next, old, true)
	next.Release()
	require.Eventually(t, func() bool { return s.Stats().GCs > 0 }, 10*time.Second, 10*time.Millisecond)
	lease, err := s.Acquire(context.Background(), s.Key("origin/repository.git:sha1"))
	require.NoError(t, err)
	defer lease.Release()
	requireObject(t, lease, old, false)
	last, err := readGCTime(lease.Dir, time.Time{})
	require.NoError(t, err)
	require.False(t, last.IsZero())
}

func TestGCBudgetFailurePreservesObjectsAndGracePins(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	now := time.Now()
	old := orphanBlob(t, l, "expired garbage", now.Add(-30*24*time.Hour))
	require.NoError(t, l.SyncRefs(context.Background(), protocol.Snapshot{Refs: []protocol.Ref{{Name: "refs/heads/main", OID: roots[len(roots)-1]}}}))
	s.mu.Lock()
	s.maxBytes = l.entry.size + 1
	s.mu.Unlock()
	changed, err := l.rewrite(context.Background(), true, now.Add(30*24*time.Hour))
	require.ErrorIs(t, err, ErrCapacity)
	require.False(t, changed)
	requireObject(t, l, old, true)
	require.True(t, l.complete(context.Background(), roots, false))
	require.Zero(t, s.Stats().ReservedBytes)
}

func TestGCRecoversGracePinFromTimestampJournal(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	now := time.Now()
	root := roots[len(roots)-1]
	// Model an interrupted ref transaction: the removal journal is durable but
	// no retention ref has been installed yet.
	require.NoError(t, atomicFile(l.Dir, retentionFile, []byte(fmt.Sprintf("{\"%s\":%d}", root, now.UnixNano()))))
	for _, oid := range roots {
		maintenanceGit(t, s.Git, l.Dir, "update-ref", "-d", "refs/cache/roots/"+oid)
	}
	expired, err := l.expireRetained(context.Background(), l.Dir, now.Add(13*24*time.Hour))
	require.NoError(t, err)
	require.Empty(t, expired)
	require.Equal(t, root, maintenanceGit(t, s.Git, l.Dir, "rev-parse", retainedPrefix+root))
}
