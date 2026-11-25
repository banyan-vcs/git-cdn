package cache

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
)

func TestBackoffDoublesAndResets(t *testing.T) {
	now := time.Now()
	var b Backoff
	require.True(t, b.Due(now), "a healthy backoff blocks the first attempt")
	b.Fail(now, time.Minute, time.Hour)
	require.False(t, b.Due(now.Add(30*time.Second)))
	require.True(t, b.Due(now.Add(time.Minute)))
	// Each failure doubles the wait, counted from that failure.
	b.Fail(now.Add(time.Minute), time.Minute, time.Hour)
	require.False(t, b.Due(now.Add(3*time.Minute-time.Millisecond)), "the second failure did not double the wait")
	require.True(t, b.Due(now.Add(3*time.Minute)))
	b.Fail(now.Add(3*time.Minute), time.Minute, time.Hour)
	require.False(t, b.Due(now.Add(7*time.Minute-time.Millisecond)), "the third failure did not double the wait again")
	require.True(t, b.Due(now.Add(7*time.Minute)))
	for range 10 {
		b.Fail(now.Add(time.Hour), time.Minute, time.Hour)
	}
	require.LessOrEqual(t, b.Wait(now.Add(time.Hour+time.Nanosecond)), time.Hour, "the wait exceeded its limit")
	b.Reset()
	require.True(t, b.Due(now), "a success did not clear the backoff")
	require.Zero(t, b.Wait(now))
}

// ancestryRepo builds a repository with two divergent lines,
//
//	base ── a1 ── a2 (annotated tag v1)     base ── side1 ── side2
//
// and returns its lease, a helper that adds commits, and the five OIDs in
// that order.
func ancestryRepo(t *testing.T) (*Lease, func(message string, parents ...string) string, []string) {
	t.Helper()
	g, err := nativegit.New("git", 1)
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 1<<20)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	dir := filepath.Join(t.TempDir(), "repo.git")
	require.NoError(t, g.InitBare(ctx, dir))
	git := func(input string, args ...string) string {
		args = append([]string{"-c", "user.name=Test", "-c", "user.email=test@example.invalid"}, args...)
		out, err := g.Output(ctx, dir, input, args...)
		require.NoError(t, err, strings.Join(args, " "))
		return strings.TrimSpace(string(out))
	}
	tree := git("", "mktree")
	commit := func(message string, parents ...string) string {
		args := []string{"commit-tree", tree, "-m", message}
		for _, parent := range parents {
			args = append(args, "-p", parent)
		}
		return git("", args...)
	}
	base := commit("base")
	a1 := commit("a1", base)
	a2 := commit("a2", a1)
	side1 := commit("side1", base)
	side2 := commit("side2", side1)
	git("", "tag", "-a", "-m", "annotated", "v1", a2)
	return &Lease{Dir: dir, store: s}, commit, []string{base, a1, a2, side1, side2}
}

// AncestorsWithin names the covered candidates instead of failing the whole
// batch, behind the same commit-only gate AncestorsAll applies.
func TestAncestorsWithinClassifiesMixedBatches(t *testing.T) {
	lease, _, history := ancestryRepo(t)
	base, a1, a2, side1, side2 := history[0], history[1], history[2], history[3], history[4]
	ctx := context.Background()
	tag := strings.TrimSpace(func() string {
		out, err := lease.store.Git.Output(ctx, lease.Dir, "", "rev-parse", "v1")
		require.NoError(t, err)
		return string(out)
	}())

	within, err := lease.AncestorsWithin(ctx, []string{base, a1, a2, side1, side2}, []string{a2})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{base: true, a1: true, a2: true}, within, "the ancestors of a2 were not reported alone")

	within, err = lease.AncestorsWithin(ctx, []string{side1}, []string{a2, side2})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{side1: true}, within, "the union of the roots must cover both lines")

	within, err = lease.AncestorsWithin(ctx, []string{tag, strings.Repeat("0", 40), a1, a1}, []string{a2})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{a1: true}, within, "a tag object, a missing object or a duplicate broke the classification")
	require.True(t, lease.AncestorsAll(ctx, []string{a1, base}, []string{a2}), "AncestorsAll disagreed with the classification")
	require.False(t, lease.AncestorsAll(ctx, []string{a1, side1}, []string{a2}), "AncestorsAll disagreed with the classification")
}

// A batch whose escaping history overflows the metadata output limit is
// bisected until every part fits; a single commit that still overflows is
// outside by construction, because a covered commit prints nothing.
func TestAncestorsWithinBisectsOversizedAnswers(t *testing.T) {
	lease, commit, history := ancestryRepo(t)
	a1, side := history[1], history[4]
	for range 40 {
		side = commit("more", side)
	}
	ctx := context.Background()
	previous := nativegit.MetadataOutputLimit
	t.Cleanup(func() { nativegit.MetadataOutputLimit = previous })
	nativegit.MetadataOutputLimit = 200
	within, err := lease.AncestorsWithin(ctx, []string{a1, side}, []string{a1})
	require.NoError(t, err)
	require.Equal(t, map[string]bool{a1: true}, within, "the covered commit was lost while bisecting")
	nativegit.MetadataOutputLimit = 16 << 20
	again, err := lease.AncestorsWithin(ctx, []string{a1, side}, []string{a1})
	require.NoError(t, err)
	require.Equal(t, within, again, "the bisected answer differed from a direct one")
}

// workHistory makes a work repository of consecutive commits and returns
// its path and the commit OIDs, oldest first.
func workHistory(t *testing.T, g *nativegit.Runner, commits int) (string, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)
	work := filepath.Join(t.TempDir(), "work")
	_, err := g.Output(ctx, "", "", "init", "-q", "--initial-branch=main", work)
	require.NoError(t, err)
	identity := map[string]string{"GIT_AUTHOR_NAME": "Cache Test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "Cache Test", "GIT_COMMITTER_EMAIL": "test@example.invalid"}
	var roots []string
	for i := range commits {
		require.NoError(t, os.WriteFile(filepath.Join(work, "data"), []byte(strings.Repeat("x", i%7)+"\n"), 0600))
		_, err = g.Output(ctx, work, "", "add", "data")
		require.NoError(t, err)
		require.NoError(t, g.Run(ctx, work, nil, io.Discard, identity, "commit", "-q", "-m", "revision"))
		tip, err := g.Output(ctx, work, "", "rev-parse", "HEAD")
		require.NoError(t, err)
		roots = append(roots, strings.TrimSpace(string(tip)))
	}
	return work, roots
}

// A hydration that fails leaves the scope in backoff: missing roots are not
// fetched again until it passes.
func TestHydrationBacksOffAfterFailure(t *testing.T) {
	g, err := nativegit.New("git", 1)
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 64<<20)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	work, roots := workHistory(t, g, 2)
	l, err := s.Acquire(context.Background(), s.Key("origin/repository.git:sha1"))
	require.NoError(t, err)
	defer l.Release()
	origin := (&url.URL{Scheme: "file", Path: work}).String()
	s.mu.Lock()
	s.maxBytes = 1
	s.mu.Unlock()
	previous := hydrationFailureBackoff
	hydrationFailureBackoff = time.Hour
	t.Cleanup(func() { hydrationFailureBackoff = previous })
	_, err = l.Ensure(context.Background(), origin, nil, []string{roots[0]})
	require.ErrorIs(t, err, ErrCapacity)
	s.mu.Lock()
	s.maxBytes = 64 << 20
	s.mu.Unlock()
	_, err = l.Ensure(context.Background(), origin, nil, []string{roots[0]})
	require.ErrorIs(t, err, ErrHydrationBackoff, "a failed hydration was retried at once")

	// A caller that already left says nothing about the scope: its failure
	// is not recorded, and the next request is not blocked by it.
	s.hydrationRecovered(l.entry.key)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = l.Ensure(canceled, origin, nil, []string{roots[1]})
	require.ErrorIs(t, err, context.Canceled)
	require.False(t, s.hydrationBlocked(l.entry.key), "a canceled caller put the scope into backoff")

	hit, err := l.Ensure(context.Background(), origin, nil, []string{roots[0]})
	require.NoError(t, err)
	require.False(t, hit)
	hit, err = l.Ensure(context.Background(), origin, nil, []string{roots[0]})
	require.NoError(t, err)
	require.True(t, hit, "hydrated roots were not reused")
	require.Zero(t, s.hydrationWait(l.entry.key), "a successful hydration left its backoff behind")
}

// Roots travel to `git fetch` on standard input, so a request naming far
// more roots than a command line holds still hydrates in one fetch.
func TestHydrationTakesRootsFromStandardInput(t *testing.T) {
	g, err := nativegit.New("git", 2)
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 64<<20)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	work, roots := workHistory(t, g, 150)
	l, err := s.Acquire(context.Background(), s.Key("origin/repository.git:sha1"))
	require.NoError(t, err)
	defer l.Release()
	origin := (&url.URL{Scheme: "file", Path: work}).String()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	hit, err := l.Ensure(ctx, origin, nil, roots)
	require.NoError(t, err)
	require.False(t, hit)
	require.True(t, l.complete(ctx, roots, false), "some of %d roots were not received", len(roots))
	hit, err = l.Ensure(ctx, origin, nil, roots)
	require.NoError(t, err)
	require.True(t, hit)
}

// A collection that cannot finish is not restarted by every scan: it backs
// off, doubling, and is retried once each wait has passed.
func TestMaintenanceBacksOffAfterFailedCollection(t *testing.T) {
	s, l, _ := populatedLease(t, 32)
	previousBase, previousLimit := maintenanceFailureBackoff, failureBackoffLimit
	maintenanceFailureBackoff, failureBackoffLimit = 80*time.Millisecond, 320*time.Millisecond
	t.Cleanup(func() { maintenanceFailureBackoff, failureBackoffLimit = previousBase, previousLimit })
	s.mu.Lock()
	l.entry.lastGC = time.Now().Add(-2 * s.repack.GCInterval)
	s.repack.GCTimeout = time.Nanosecond
	s.mu.Unlock()
	l.Release()
	require.Eventually(t, func() bool { return s.Stats().GCFailed > 0 }, 10*time.Second, 10*time.Millisecond, "the due collection never ran")
	// Offer the scope again the way the scans and later requests do. Waits
	// of 80, 160, 320, 320 ... ms allow a handful of attempts in a second,
	// where an un-backed-off collection was restarted on every offer.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		s.queueRepackLocked(l.entry)
		s.mu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	failed := s.Stats().GCFailed
	require.GreaterOrEqual(t, failed, uint64(3), "failed collections were not retried after their backoff")
	require.LessOrEqual(t, failed, uint64(7), "a failing collection was restarted without backoff: %d attempts", failed)
	require.Zero(t, s.Stats().GCs)
}

// Collection rebuilds the commit-graph and installs it by rename. Commits
// that arrive afterwards are read from the object store next to the stale
// graph, and the next collection replaces the read-only file.
func TestCollectionRebuildsCommitGraph(t *testing.T) {
	s, l, roots := populatedLease(t, 32)
	defer l.Release()
	graph := filepath.Join(l.Dir, "objects", "info", "commit-graph")
	_, err := os.Stat(graph)
	require.True(t, os.IsNotExist(err), "a commit-graph existed before any collection")
	changed, err := l.rewrite(context.Background(), true, time.Now())
	require.NoError(t, err)
	require.True(t, changed)
	_, err = os.Stat(graph)
	require.NoError(t, err, "collection did not write a commit-graph")
	require.Equal(t, "10", maintenanceGit(t, s.Git, l.Dir, "rev-list", "--count", "--all", "--missing=error"))
	maintenanceGit(t, s.Git, l.Dir, "commit-graph", "verify")
	require.True(t, l.AncestorsAll(context.Background(), roots[:9], roots[9:]), "ancestry checks fail with a graph present")
	require.True(t, l.complete(context.Background(), roots, false), "the completeness walk fails with a graph present")

	// History the graph does not know about yet.
	tree := maintenanceGit(t, s.Git, l.Dir, "mktree")
	var output bytes.Buffer
	require.NoError(t, s.Git.Run(context.Background(), l.Dir, nil, &output, map[string]string{"GIT_AUTHOR_NAME": "Cache Test", "GIT_AUTHOR_EMAIL": "test@example.invalid"}, "commit-tree", tree, "-m", "after the graph", "-p", roots[9]))
	later := strings.TrimSpace(output.String())
	require.True(t, l.AncestorsAll(context.Background(), roots, []string{later}), "commits outside the graph broke ancestry checks")
	require.False(t, l.AncestorsAll(context.Background(), []string{later}, roots), "a commit outside every root was covered")

	changed, err = l.rewrite(context.Background(), true, time.Now())
	require.NoError(t, err, "replacing the published read-only graph failed")
	require.True(t, changed)
	maintenanceGit(t, s.Git, l.Dir, "commit-graph", "verify")
	require.True(t, l.complete(context.Background(), roots, false))
}

// A request that already gave up does not cancel the maintenance it would
// have preempted, and still releases its pin.
func TestCanceledWaiterDoesNotPreemptMaintenance(t *testing.T) {
	s := testStore(t, 1<<20)
	e := newEntry(s.Key("a"))
	s.mu.Lock()
	s.entries[e.key] = e
	e.users++
	e.writer = true
	var cancels atomic.Int32
	e.cancelMaint = func() { cancels.Add(1) }
	s.mu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.AcquireShared(ctx, e.key)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, cancels.Load(), "a request that had already given up cancelled maintenance")
	s.mu.Lock()
	require.Equal(t, 1, e.users, "a canceled waiter kept its pin")
	e.cancelMaint = nil
	e.writer = false
	e.users--
	s.mu.Unlock()
}

// Eviction moves the scope directory aside under the mutex and deletes the
// contents in the background: the managed tree is consistent at once, and
// no request waits for another repository's files to disappear.
func TestEvictionDeletesScopeContentsInTheBackground(t *testing.T) {
	s := testStore(t, 100)
	a := storedBytes(t, s, "a", 60)
	b := storedBytes(t, s, "b", 40)
	c := testLease(t, s, "c")
	require.NoError(t, publishBytes(t, c, 50))
	c.Release()
	require.NoDirExists(t, a, "the evicted scope stayed in the managed tree")
	require.DirExists(t, b, "eviction removed a scope that was not due")
	require.EqualValues(t, 90, s.Stats().Bytes, "budget after eviction")
	require.Eventually(t, func() bool {
		entries, err := os.ReadDir(s.Tmp)
		if err != nil {
			return false
		}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), "evicted-") {
				return false
			}
		}
		return true
	}, 10*time.Second, 10*time.Millisecond, "evicted contents were not deleted")
}
