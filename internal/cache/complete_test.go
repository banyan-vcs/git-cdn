package cache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"

	"github.com/stretchr/testify/require"
)

// Complete consults the refs/cache index before walking the object graph:
// a want inside an indexed closure skips the walk, and anything else falls
// back to the authoritative rev-list over the real objects.
func TestCompleteUsesIndexedRootsBeforeWalking(t *testing.T) {
	g, err := nativegit.New("git", 1)
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 1<<20)
	require.NoError(t, err)
	defer s.Close()
	dir := filepath.Join(t.TempDir(), "repo.git")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, g.InitBare(ctx, dir))
	run := func(input string, args ...string) string {
		out, err := g.Output(ctx, dir, input, append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)...)
		require.NoError(t, err, strings.Join(args, " "))
		return strings.TrimSpace(string(out))
	}
	tree := run("", "mktree")
	commit := func(message string, parents ...string) string {
		args := []string{"commit-tree", tree, "-m", message}
		for _, parent := range parents {
			args = append(args, "-p", parent)
		}
		return run("", args...)
	}
	a1 := commit("a1")
	a2 := commit("a2", a1)
	side := commit("side", a1)
	lease := &Lease{Dir: dir, store: s, entry: newEntry("indexed")}
	lease.entry.size = rootsIndexMinScopeBytes

	require.False(t, lease.rootsCover(ctx, []string{a2}), "no indexed refs yet")
	require.True(t, lease.Complete(ctx, []string{a2}), "fallback walk sees the real objects")

	run("", "update-ref", "refs/cache/roots/"+a1, a1)
	require.True(t, lease.rootsCover(ctx, []string{a1}), "want equal to an indexed root is covered")
	require.False(t, lease.rootsCover(ctx, []string{a2}), "want beyond the indexed root is not covered")

	run("", "update-ref", "refs/cache/roots/"+a2, a2)
	require.True(t, lease.rootsCover(ctx, []string{a2}), "ancestor of an indexed root is covered")
	require.True(t, lease.rootsCover(ctx, []string{a1, a2}), "every want must be covered")
	require.False(t, lease.rootsCover(ctx, []string{side}), "sibling history outside the index")

	run("", "update-ref", "-d", "refs/cache/roots/"+a2)
	run("", "update-ref", "-d", "refs/cache/roots/"+a1)
	require.True(t, lease.Complete(ctx, []string{a2}), "without index entries the walk decides")
}
