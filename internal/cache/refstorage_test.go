package cache

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

func TestNewCachesAndViewsUseReftable(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	require.Equal(t, "reftable", maintenanceGit(t, s.Git, l.Dir, "config", "--get", "extensions.refStorage"))
	root := roots[len(roots)-1]
	for _, target := range []string{"", "refs/heads/main"} {
		snapshot := protocol.Snapshot{Refs: []protocol.Ref{{Name: "HEAD", OID: root, Symref: target}, {Name: "refs/heads/main", OID: root}}}
		view, err := l.View(context.Background(), snapshot, []string{root})
		require.NoError(t, err)
		require.Equal(t, "reftable", maintenanceGit(t, s.Git, view, "config", "--get", "extensions.refStorage"))
		head, err := s.Git.Head(context.Background(), view)
		require.NoError(t, err)
		if target == "" {
			require.Equal(t, root+"\n", head)
		} else {
			require.Equal(t, "ref: "+target+"\n", head)
		}
		require.Equal(t, root, maintenanceGit(t, s.Git, view, "rev-parse", "HEAD"))
		require.NoError(t, os.RemoveAll(view))
	}
}

func TestExistingFilesCacheKeepsObjectsAndRefBackend(t *testing.T) {
	s, l, roots := populatedLease(t, 32, "files")
	defer l.Release()
	root := roots[len(roots)-1]
	hit, err := l.EnsureWith(context.Background(), []string{root}, func(context.Context, string, []string) error {
		t.Fatal("existing files cache unexpectedly fetched objects")
		return nil
	})
	require.NoError(t, err)
	require.True(t, hit)
	snapshot := protocol.Snapshot{Refs: []protocol.Ref{{Name: "HEAD", OID: root, Symref: "refs/heads/main"}, {Name: "refs/heads/main", OID: root}}}
	require.NoError(t, l.SyncRefs(context.Background(), snapshot))
	require.Equal(t, "refs/cache/upstream/heads/main", maintenanceGit(t, s.Git, l.Dir, "symbolic-ref", "HEAD"))
	_, err = os.Stat(filepath.Join(l.Dir, "reftable"))
	require.ErrorIs(t, err, os.ErrNotExist)
	require.True(t, l.complete(context.Background(), roots, false))
	s.repack.PackThreshold = 2
	_, err = l.Repack(context.Background())
	require.NoError(t, err)
	_, err = l.Collect(context.Background())
	require.NoError(t, err)
	require.True(t, l.complete(context.Background(), roots, false))
}

func TestReftableRefBudgetFailurePreservesRefsAndHead(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	before := maintenanceGit(t, s.Git, l.Dir, "for-each-ref", "--format=%(refname) %(objectname)")
	head, err := s.Git.Head(context.Background(), l.Dir)
	require.NoError(t, err)
	s.mu.Lock()
	s.maxBytes = l.entry.size + 64
	s.mu.Unlock()
	root := roots[len(roots)-1]
	err = l.SyncRefs(context.Background(), protocol.Snapshot{Refs: []protocol.Ref{{Name: "HEAD", OID: root, Symref: "refs/heads/main"}, {Name: "refs/heads/main", OID: root}}})
	require.ErrorIs(t, err, ErrCapacity)
	require.Equal(t, before, maintenanceGit(t, s.Git, l.Dir, "for-each-ref", "--format=%(refname) %(objectname)"))
	after, err := s.Git.Head(context.Background(), l.Dir)
	require.NoError(t, err)
	require.Equal(t, head, after)
	require.True(t, l.complete(context.Background(), roots, false))
	require.Zero(t, s.Stats().ReservedBytes)
}

func TestRefBudgetCoversLargeReftableTransactions(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	for _, remove := range []bool{false, true} {
		var updates strings.Builder
		for i := range 5000 {
			if remove {
				fmt.Fprintf(&updates, "delete refs/cache/upstream/heads/branch-%05d\n", i)
			} else {
				fmt.Fprintf(&updates, "update refs/cache/upstream/heads/branch-%05d %s\n", i, roots[i%len(roots)])
			}
		}
		budget, err := refUpdateBudget(l.Dir, updates.Len())
		require.NoError(t, err)
		before, err := directorySize(filepath.Join(l.Dir, "reftable"))
		require.NoError(t, err)
		require.NoError(t, s.Git.Run(context.Background(), l.Dir, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin"))
		after, err := directorySize(filepath.Join(l.Dir, "reftable"))
		require.NoError(t, err)
		require.LessOrEqual(t, after-before, budget)
	}
}
