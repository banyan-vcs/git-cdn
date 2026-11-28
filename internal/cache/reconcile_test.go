package cache

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"

	"github.com/stretchr/testify/require"
)

type reconcileSetup struct {
	store         *Store
	key           string
	first, second string
	origin        string
	snapshotFor   func(string) protocol.Snapshot
}

// reconcileFixture builds a store with a two-commit file origin, hydrates
// the first tip and releases the lease, leaving an idle scope whose next
// hydration target is the second tip.
func reconcileFixture(t *testing.T, options ...nativegit.Option) reconcileSetup {
	t.Helper()
	g, err := nativegit.New("git", 4, options...)
	require.NoError(t, err)
	s, err := Open(t.TempDir(), g, 64<<20, RepackOptions{PackThreshold: 32, Timeout: 30 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	ctx := context.Background()
	work := filepath.Join(t.TempDir(), "work")
	maintenanceGit(t, g, "", "init", "-q", "--object-format=sha1", "--initial-branch=main", work)
	maintenanceGit(t, g, work, "commit", "-q", "--allow-empty", "-m", "first")
	first := maintenanceGit(t, g, work, "rev-parse", "HEAD")
	maintenanceGit(t, g, work, "commit", "-q", "--allow-empty", "-m", "second")
	second := maintenanceGit(t, g, work, "rev-parse", "HEAD")
	key := s.Key("origin/repository.git:sha1")
	l, err := s.Acquire(ctx, key)
	require.NoError(t, err)
	origin := (&url.URL{Scheme: "file", Path: work}).String()
	_, err = l.Ensure(ctx, origin, nil, []string{first})
	require.NoError(t, err)
	l.Release()
	// Snapshots must carry a real parse-time fingerprint for the debounce
	// gates, so build them the way the server does.
	snapshotFor := func(tip string) protocol.Snapshot {
		data := append(protocol.Encode(tip+" refs/heads/main\n"), []byte("0000")...)
		snapshot, err := protocol.ParseRefs(data)
		require.NoError(t, err)
		snapshot.ObservedAt = time.Now()
		return snapshot
	}
	return reconcileSetup{store: s, key: key, first: first, second: second, origin: origin, snapshotFor: snapshotFor}
}

func (r reconcileSetup) cacheRefs(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(r.store.Root, "repos", r.key, "repo.git")
	return strings.Fields(maintenanceGit(t, r.store.Git, dir, "for-each-ref", "--format=%(refname)", "refs/cache/"))
}

// Snapshot drift retires roots: a tip that leaves the advertised set moves
// into the retention grace period instead of pinning its objects forever,
// while hydration of the new tip pins a fresh root and the retired objects
// keep serving through the grace period.
func TestReconcileRetiresRootsOnSnapshotDrift(t *testing.T) {
	setup := reconcileFixture(t)
	ctx := context.Background()
	l, err := setup.store.Acquire(ctx, setup.key)
	require.NoError(t, err)
	require.NoError(t, l.SyncRefs(ctx, setup.snapshotFor(setup.first)))
	refs := setup.cacheRefs(t)
	require.Contains(t, refs, "refs/cache/roots/"+setup.first, "hydrated tip was not pinned")
	require.Contains(t, refs, "refs/cache/upstream/heads/main", "sync did not name the upstream tip")

	_, err = l.Ensure(ctx, setup.origin, nil, []string{setup.second})
	require.NoError(t, err, "rehydration of the new tip failed")
	l.Release()
	setup.store.Reconcile(setup.key, setup.snapshotFor(setup.second))
	require.Eventually(t, func() bool {
		joined := strings.Join(setup.cacheRefs(t), " ")
		return strings.Contains(joined, "refs/cache/roots/"+setup.second) &&
			!strings.Contains(joined, "refs/cache/roots/"+setup.first)
	}, 10*time.Second, 50*time.Millisecond, "snapshot drift did not retire the old root")
	require.Contains(t, setup.cacheRefs(t), "refs/cache/retained/"+setup.first, "retired root did not enter the grace period")
	require.Eventually(t, func() bool {
		stats := setup.store.Stats()
		return stats.Reconciles >= 1 && stats.RootsRetired >= 1
	}, 10*time.Second, 20*time.Millisecond, "background reconciliation was not counted")

	warm, err := setup.store.AcquireShared(ctx, setup.key)
	require.NoError(t, err)
	require.True(t, warm.Complete(ctx, []string{setup.first}), "retired root lost its object closure")
	warm.Release()
}

// Reconciliation is debounced: an unchanged fingerprint with no new
// hydrations never spawns another run, while accumulated hydrations force
// one even without drift.
func TestReconcileDebounceGates(t *testing.T) {
	setup := reconcileFixture(t)
	setup.store.Reconcile(setup.key, setup.snapshotFor(setup.first))
	require.Eventually(t, func() bool { return setup.store.Stats().Reconciles >= 1 }, 10*time.Second, 50*time.Millisecond, "first reconciliation never ran")
	// Drain any spawn that raced the first completion before measuring.
	time.Sleep(300 * time.Millisecond)
	before := setup.store.Stats().Reconciles
	same := setup.snapshotFor(setup.first)
	for range 3 {
		setup.store.Reconcile(setup.key, same)
	}
	time.Sleep(300 * time.Millisecond)
	require.Equal(t, before, setup.store.Stats().Reconciles, "unchanged snapshot reconciled again")

	setup.store.mu.Lock()
	setup.store.entries[setup.key].hydrateSince = reconcileHydrationDelta
	setup.store.mu.Unlock()
	setup.store.Reconcile(setup.key, same)
	require.Eventually(t, func() bool { return setup.store.Stats().Reconciles > before }, 10*time.Second, 50*time.Millisecond, "accumulated hydrations did not force a reconcile")
}

// A busy scope defers reconciliation instead of queueing behind its
// readers; once idle, the pending snapshot applies.
func TestReconcileDefersWhileScopeIsBusy(t *testing.T) {
	setup := reconcileFixture(t)
	shared, err := setup.store.AcquireShared(context.Background(), setup.key)
	require.NoError(t, err)
	setup.store.Reconcile(setup.key, setup.snapshotFor(setup.first))
	time.Sleep(600 * time.Millisecond)
	require.EqualValues(t, 0, setup.store.Stats().Reconciles, "reconciliation ran while the scope was busy")
	shared.Release()
	require.Eventually(t, func() bool { return setup.store.Stats().Reconciles >= 1 }, 10*time.Second, 50*time.Millisecond, "deferred reconciliation never ran once idle")
}

// A request preempts reconciliation so it can hydrate the new tip. The
// cancellation must not put that request's newer snapshot into failure
// backoff, or the old snapshot's retry becomes the last applied ref view.
func TestReconcilePreemptionDoesNotBackoffSnapshotDrift(t *testing.T) {
	setup := reconcileFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var holdOnce, releaseOnce sync.Once
	reconcileHold = func() {
		holdOnce.Do(func() {
			close(entered)
			<-release
		})
	}
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		require.NoError(t, setup.store.Close())
		reconcileHold = nil
	})
	setup.store.Reconcile(setup.key, setup.snapshotFor(setup.first))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation never acquired its lease")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acquired := make(chan *Lease, 1)
	acquireErr := make(chan error, 1)
	go func() {
		lease, err := setup.store.Acquire(ctx, setup.key)
		if err != nil {
			acquireErr <- err
			return
		}
		acquired <- lease
	}()
	require.Eventually(t, func() bool {
		setup.store.mu.Lock()
		defer setup.store.mu.Unlock()
		return setup.store.entries[setup.key].writers == 1
	}, 5*time.Second, 10*time.Millisecond, "request never preempted reconciliation")
	releaseOnce.Do(func() { close(release) })
	var lease *Lease
	select {
	case lease = <-acquired:
	case err := <-acquireErr:
		require.NoError(t, err)
		return
	case <-ctx.Done():
		t.Fatal("request never acquired its lease after preemption")
	}
	defer func() {
		if lease != nil {
			lease.Release()
		}
	}()
	_, err := lease.Ensure(ctx, setup.origin, nil, []string{setup.second})
	require.NoError(t, err)
	lease.Release()
	lease = nil
	setup.store.Reconcile(setup.key, setup.snapshotFor(setup.second))
	require.Eventually(t, func() bool {
		refs := setup.cacheRefs(t)
		return strings.Contains(strings.Join(refs, " "), "refs/cache/roots/"+setup.second) &&
			!strings.Contains(strings.Join(refs, " "), "refs/cache/roots/"+setup.first)
	}, 10*time.Second, 50*time.Millisecond, "preempted reconciliation lost the new snapshot")
	require.Contains(t, setup.cacheRefs(t), retainedPrefix+setup.first)
}
