package server

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A shallow fetch serves locally and enters the response pack cache: two
// depth=1 fetches of the same tip cost one origin fetch and one generation,
// and the second replays the first verbatim.
func TestShallowDepthOneFetchesShareOneGeneration(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip, "deepen 1")
	first := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	firstPayload := responseBody(t, first)
	require.EqualValues(t, "MISS", first.Header.Get("X-GitCDN-Cache-Status"), "first shallow fetch was not served locally")
	require.EqualValues(t, 1, f.fetches.Load(), "shallow fetches hydrated more than once")

	second := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	secondPayload := responseBody(t, second)
	require.EqualValues(t, "HIT", second.Header.Get("X-GitCDN-Cache-Status"), "second shallow fetch was not served from the object cache")
	require.EqualValues(t, "HIT", second.Header.Get("X-GitCDN-Pack-Cache"), "second shallow fetch was not a pack-cache hit")
	require.Equal(t, firstPayload, secondPayload, "shallow responses diverged between generations")
	require.EqualValues(t, 1, f.fetches.Load(), "second shallow fetch returned to the origin")
	require.EqualValues(t, 2, s.local.Load(), "shallow fetches were not served locally")
}

// A filtered fetch follows the same rule as a shallow one: identical
// request bytes and snapshot replay verbatim.
func TestFilteredFetchesShareOneGeneration(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip, "filter blob:none")
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	second := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	payload := responseBody(t, second)
	require.EqualValues(t, "HIT", second.Header.Get("X-GitCDN-Pack-Cache"), "second filtered fetch was not a pack-cache hit")
	require.EqualValues(t, 1, f.fetches.Load(), "filtered fetches hydrated more than once")
	require.EqualValues(t, 2, s.local.Load(), "filtered fetches were not served locally")
	require.NotEmpty(t, payload)
}

// Shallow traffic reconciles storage refs in the background too: every
// request carries a complete live snapshot from its probe, and the
// debounced idle-only reconcile names the upstream refs without ever
// blocking the response path.
func TestShallowFetchesReconcileInBackground(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, "deepen 1"), testAuthorization, nil))
	repos, err := filepath.Glob(filepath.Join(s.store.Root, "repos", "*", "repo.git"))
	require.NoError(t, err)
	require.NotEmpty(t, repos, "no cached repository was created")
	require.EqualValues(t, 1, len(repos))
	require.Eventually(t, func() bool {
		return metricValue(t, h, "gitcdn_reconciles_total") >= 1
	}, 15*time.Second, 50*time.Millisecond, "shallow traffic never reconciled")
	require.Contains(t, testGit(t, repos[0], "for-each-ref", "--format=%(refname)", "refs/cache/upstream/"), "refs/cache/upstream/heads/main", "reconcile did not name the upstream tip")
}

// End-to-end prune: after the upstream tip moves, the next shallow fetch
// retires the old root into the retention grace period, so the scope stops
// pinning history nothing advertises.
func TestShallowFleetRetiresOldRootsAfterDrift(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, "deepen 1"), testAuthorization, nil))
	repos, err := filepath.Glob(filepath.Join(s.store.Root, "repos", "*", "repo.git"))
	require.NoError(t, err)
	require.EqualValues(t, 1, len(repos))
	oldTip := f.tip
	f.commit(t, "drift")
	// Move main and drop the tag, so the old tip leaves the advertised set
	// entirely; a tag still pointing at it would legitimately keep it.
	testGit(t, f.work, "push", "--quiet", f.repo, "main", ":refs/tags/v1")
	body := fetchBody(f.tip, "deepen 1")
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	// The fetch that hydrated the drifted tip schedules the reconciliation
	// once: a replay of it is served from the response pack cache without
	// reaching the scope's gate, so this waits for that one background run.
	require.Eventually(t, func() bool {
		joined := testGit(t, repos[0], "for-each-ref", "--format=%(refname)", "refs/cache/")
		return strings.Contains(joined, "refs/cache/roots/"+f.tip) && !strings.Contains(joined, "refs/cache/roots/"+oldTip)
	}, 60*time.Second, 100*time.Millisecond, "snapshot drift did not retire the old root")
	require.Contains(t, testGit(t, repos[0], "for-each-ref", "--format=%(refname)", "refs/cache/retained/"), "refs/cache/retained/"+oldTip, "retired root did not enter the grace period")
	// The count is taken once the ref transaction and the HEAD write after it
	// have both ended; the refs above become visible first.
	require.Eventually(t, func() bool {
		return metricValue(t, h, "gitcdn_roots_retired_total") >= 1
	}, 15*time.Second, 50*time.Millisecond, "retirement was not counted")
}

// A reconciliation preempted while Git held the reference lock can leave the
// lock behind; from then on every reference transaction in the scope fails.
// The drifted tip must still be hydrated locally — a failure there sends the
// fetch to the upstream instead — and the old root must still retire.
func TestShallowFleetRetiresOldRootsAfterAKilledRefWriter(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, "deepen 1"), testAuthorization, nil))
	repos, err := filepath.Glob(filepath.Join(s.store.Root, "repos", "*", "repo.git"))
	require.NoError(t, err)
	require.EqualValues(t, 1, len(repos))
	// Wait for the first reconciliation: a lock planted under its running
	// transaction would fail that one, and its backoff is not what this
	// test is about.
	require.Eventually(t, func() bool {
		return metricValue(t, h, "gitcdn_reconciles_total") >= 1
	}, 15*time.Second, 50*time.Millisecond, "shallow traffic never reconciled")
	oldTip := f.tip
	f.commit(t, "drift")
	testGit(t, f.work, "push", "--quiet", f.repo, "main", ":refs/tags/v1")
	require.NoError(t, os.WriteFile(filepath.Join(repos[0], "reftable", "tables.list.lock"), nil, 0600))
	drifted := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, "deepen 1"), testAuthorization, nil)
	responseBody(t, drifted)
	require.EqualValues(t, "MISS", drifted.Header.Get("X-GitCDN-Cache-Status"), "the drifted tip was not hydrated locally")
	require.Eventually(t, func() bool {
		joined := testGit(t, repos[0], "for-each-ref", "--format=%(refname)", "refs/cache/")
		return strings.Contains(joined, "refs/cache/roots/"+f.tip) && !strings.Contains(joined, "refs/cache/roots/"+oldTip)
	}, 15*time.Second, 100*time.Millisecond, "snapshot drift did not retire the old root")
}

// A batch of concurrent shallow fetches on a warm scope collapses into one
// generation through the shared-lease single flight; every client receives
// the same response bytes.
func TestConcurrentShallowBatchServesOneGeneration(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	const clients = 6
	body := fetchBody(f.tip, "deepen 1")
	// Warm the scope with a full clone so the batch measures generation
	// sharing on a warm scope, where every request holds a reader lease.
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	payloads := make([][]byte, clients)
	var wg sync.WaitGroup
	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			payloads[i] = responseBody(t, r)
		}()
	}
	wg.Wait()
	for i := 1; i < clients; i++ {
		require.Equal(t, payloads[0], payloads[i], "concurrent shallow responses diverged")
	}
	require.EqualValues(t, 1, f.fetches.Load(), "shallow batch on a warm scope returned to the origin")
	require.EqualValues(t, 1+clients, s.local.Load(), "shallow batch was not served locally")
	require.GreaterOrEqual(t, metricValue(t, h, "gitcdn_generation_flight_waits_total"), 1, "identical shallow requests did not share one generation")
}

// Real clients cloning with --depth=1 through the CDN produce working
// shallow repositories while the origin serves each pack exactly once.
func TestRealDepthOneClonesThroughCDN(t *testing.T) {
	f := newFixture(t)
	_, h := newCDN(t, f, nil)
	first := filepath.Join(t.TempDir(), "first")
	testGit(t, "", "clone", "--quiet", "--depth=1", h.URL+"/gitlab/Group/Repo.git", first)
	require.EqualValues(t, f.tip, testGit(t, first, "rev-parse", "HEAD"), "shallow clone tip mismatch")
	_, err := os.Stat(filepath.Join(first, ".git", "shallow"))
	require.NoError(t, err, "depth=1 clone is not shallow")
	require.EqualValues(t, "commit", testGit(t, first, "cat-file", "-t", "HEAD"))
	// The clone's include-tag expands hydration to the tag object; the
	// shallow pack itself never returns to the origin.
	hydrations := f.fetches.Load()

	second := filepath.Join(t.TempDir(), "second")
	testGit(t, "", "clone", "--quiet", "--depth=1", h.URL+"/gitlab/Group/Repo.git", second)
	require.EqualValues(t, f.tip, testGit(t, second, "rev-parse", "HEAD"), "second shallow clone tip mismatch")
	require.EqualValues(t, hydrations, f.fetches.Load(), "second shallow clone returned to the origin")
}

// A real partial clone with --filter=blob:none round-trips through local
// serving, and on-demand blob fetches for a checkout still work. Lazy blob
// reads want unadvertised objects, so they proxy to the origin by design;
// the filtered pack itself is generated once and replayed.
func TestRealFilteredCloneThroughCDN(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	clone := filepath.Join(t.TempDir(), "partial")
	testGit(t, "", "clone", "--quiet", "--filter=blob:none", h.URL+"/gitlab/Group/Repo.git", clone)
	require.EqualValues(t, f.tip, testGit(t, clone, "rev-parse", "HEAD"), "filtered clone tip mismatch")
	data := testGit(t, clone, "show", "HEAD:file.txt")
	require.Equal(t, "initial", data, "lazy blob fetch broke checkout content")
	hydrations := f.fetches.Load()
	stores := metricValue(t, h, "gitcdn_pack_cache_stores_total")

	second := filepath.Join(t.TempDir(), "partial2")
	testGit(t, "", "clone", "--quiet", "--filter=blob:none", h.URL+"/gitlab/Group/Repo.git", second)
	require.EqualValues(t, f.tip, testGit(t, second, "rev-parse", "HEAD"), "second filtered clone tip mismatch")
	require.Equal(t, "initial", testGit(t, second, "show", "HEAD:file.txt"), "second filtered clone checkout diverged")
	require.EqualValues(t, 1, f.fetches.Load()-hydrations, "second filtered clone did more than the lazy blob read")
	require.EqualValues(t, stores, metricValue(t, h, "gitcdn_pack_cache_stores_total"), "second filtered clone regenerated the pack")
	require.EqualValues(t, 2, s.local.Load(), "filtered clone packs were not served locally")
}
