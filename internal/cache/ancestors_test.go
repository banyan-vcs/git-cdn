package cache

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/nativegit"

	"github.com/stretchr/testify/require"
)

// AncestorsAll must decide the whole batch with one rev-list and stay
// equivalent to the earlier per-pair merge-base probes: a candidate is
// covered when it is an ancestor of (or equal to) at least one root.
func TestAncestorsAllDecidesWholeBatches(t *testing.T) {
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
	a3 := commit("a3", a2)
	side := commit("side", a1) // sibling of a2: reachable from neither a2 nor a3
	run("", "tag", "-a", "-m", "annotated", "v1", a2)
	tag := run("", "rev-parse", "v1")
	blob := run("payload\n", "hash-object", "-w", "-t", "blob", "--stdin")

	lease := &Lease{Dir: dir, store: s}
	require.True(t, lease.AncestorsAll(ctx, []string{a1}, []string{a3}), "ancestor is covered")
	require.True(t, lease.AncestorsAll(ctx, []string{a3}, []string{a3}), "equal OIDs count as covered")
	require.True(t, lease.AncestorsAll(ctx, []string{a1, a2}, []string{a3, side}), "union of roots covers both")
	require.False(t, lease.AncestorsAll(ctx, []string{tag}, []string{a3}), "tag object candidate is rejected like the old commit-only gate")
	require.False(t, lease.AncestorsAll(ctx, []string{side}, []string{a3}), "sibling history is outside")
	require.False(t, lease.AncestorsAll(ctx, []string{a1, side}, []string{a3}), "one uncovered candidate fails the batch")
	require.False(t, lease.AncestorsAll(ctx, []string{blob}, []string{a3}), "non-commit candidate fails")
	require.False(t, lease.AncestorsAll(ctx, []string{strings.Repeat("0", 40)}, []string{a3}), "missing object fails")
	require.True(t, lease.AncestorsAll(ctx, nil, []string{a3}), "no candidates trivially pass")
	require.False(t, lease.AncestorsAll(ctx, []string{a1}, nil), "no roots cannot cover anything")
}
