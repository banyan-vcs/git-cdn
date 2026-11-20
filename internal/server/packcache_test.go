package server

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/chiyutianyi/git-cdn/internal/config"

	"github.com/stretchr/testify/require"
)

func metricValue(t *testing.T, h *httptest.Server, name string) int {
	t.Helper()
	r, err := http.Get(h.URL + "/metrics")
	require.NoError(t, err)
	defer r.Body.Close()
	scanner := bufio.NewScanner(r.Body)
	for scanner.Scan() {
		if value, ok := strings.CutPrefix(scanner.Text(), name+" "); ok {
			n, err := strconv.Atoi(strings.TrimSpace(value))
			require.NoError(t, err)
			return n
		}
	}
	require.NoError(t, scanner.Err())
	return 0
}

func TestPackCacheReusesIdenticalRequests(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	r1 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	b1 := responseBody(t, r1)
	require.EqualValues(t, "MISS", r1.Header.Get("X-GitCDN-Cache-Status"))
	require.EqualValues(t, "MISS", r1.Header.Get("X-GitCDN-Pack-Cache"))
	r2 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	b2 := responseBody(t, r2)
	require.EqualValues(t, "HIT", r2.Header.Get("X-GitCDN-Cache-Status"))
	require.EqualValues(t, "HIT", r2.Header.Get("X-GitCDN-Pack-Cache"))
	require.EqualValues(t, b1, b2, "reused response differs from the generated one")
	require.EqualValues(t, 2, f.probes.Load(), "reused response skipped live authorization")
	require.EqualValues(t, 1, f.fetches.Load(), "reused response hydrated twice")
	require.EqualValues(t, 2, s.local.Load())
	require.EqualValues(t, 1, metricValue(t, h, "gitcdn_pack_cache_hits_total"))
	require.EqualValues(t, 1, metricValue(t, h, "gitcdn_pack_cache_stores_total"))
	// Incremental fetches carry client haves and never touch the pack cache.
	r3 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, "have "+f.tip), testAuthorization, nil)
	responseBody(t, r3)
	require.EqualValues(t, 1, metricValue(t, h, "gitcdn_pack_cache_hits_total"))
}

func TestPackCacheKeyFollowsSnapshot(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip, "include-tag")
	r1 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r1)
	f.commit(t, "second")
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	testGit(t, f.work, "tag", "-a", "v2", "-m", "second release")
	testGit(t, f.work, "push", "--quiet", f.repo, "refs/tags/v2")
	// Same want set, but the snapshot changed (new tip, new tag): the key
	// must miss and the response must be regenerated. The old tip stays
	// advertised through the peeled v1 tag, so it remains authorized — the
	// guard is the snapshot fingerprint, not ref deletion.
	r2 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r2)
	require.EqualValues(t, "MISS", r2.Header.Get("X-GitCDN-Pack-Cache"), "stale snapshot reused a response")
	require.EqualValues(t, 0, s.packHits.Load())
}

func TestPackCacheDisabledByNegativeConfig(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxPackCacheBytes = -1 })
	body := fetchBody(f.tip)
	r1 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r1)
	require.Empty(t, r1.Header.Get("X-GitCDN-Pack-Cache"))
	r2 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r2)
	require.Empty(t, r2.Header.Get("X-GitCDN-Pack-Cache"))
	require.EqualValues(t, "HIT", r2.Header.Get("X-GitCDN-Cache-Status"))
	require.EqualValues(t, 0, s.packStores.Load())
}

func TestPackCacheBudgetDropsOversizedResponses(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxPackCacheBytes = 1 })
	body := fetchBody(f.tip)
	r1 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r1)
	r2 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r2)
	require.EqualValues(t, "MISS", r2.Header.Get("X-GitCDN-Pack-Cache"), "oversized response stayed cached")
	require.EqualValues(t, 0, s.packStores.Load())
}
