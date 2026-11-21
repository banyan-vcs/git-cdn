package server

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestPackCacheRegeneratesTruncatedEntries(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	r1 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r1)
	require.EqualValues(t, 1, s.packStores.Load())
	err := filepath.WalkDir(s.store.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.Contains(filepath.ToSlash(path), "/packcache/") {
			return err
		}
		return os.Truncate(path, 16)
	})
	require.NoError(t, err)
	r2 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	b2 := responseBody(t, r2)
	require.EqualValues(t, "MISS", r2.Header.Get("X-GitCDN-Pack-Cache"), "truncated entry was served")
	require.EqualValues(t, "HIT", r2.Header.Get("X-GitCDN-Cache-Status"))
	require.True(t, len(b2) > 16, "regenerated response is truncated")
}

func TestPackCacheServesConcurrentIdenticalRequests(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	const requests = 8
	bodies := make([][]byte, requests)
	statuses := make([]string, requests)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			bodies[i] = responseBody(t, r)
			statuses[i] = r.Header.Get("X-GitCDN-Pack-Cache")
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, s.packStores.Load(), "concurrent identical clones generated %d packs", s.packStores.Load())
	require.EqualValues(t, requests-1, s.packHits.Load(), "concurrent identical clones did not reuse the response")
	generated := 0
	for i := range requests {
		require.Contains(t, []string{"MISS", "HIT"}, statuses[i], "request %d: unexpected status %q", i, statuses[i])
		if statuses[i] == "MISS" {
			generated++
		}
		require.EqualValues(t, bodies[0], bodies[i], "request %d received a different response", i)
	}
	require.EqualValues(t, 1, generated, "concurrent identical clones generated multiple responses")
}

func TestPackCacheHitSkipsRepositoryGate(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r)
	require.EqualValues(t, 1, s.packStores.Load())
	identity := f.origin.URL + "/root/Group/Repo.git:sha1"
	lease, err := s.store.Acquire(context.Background(), s.store.Key(identity))
	require.NoError(t, err)
	defer lease.Release()
	// The warm response must stream while another holder keeps the
	// repository gate: the fast path pins instead of acquiring.
	done := make(chan *http.Response, 1)
	go func() {
		done <- postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	}()
	select {
	case warm := <-done:
		require.EqualValues(t, "HIT", warm.Header.Get("X-GitCDN-Pack-Cache"))
		require.NotEmpty(t, responseBody(t, warm))
	case <-time.After(10 * time.Second):
		require.FailNow(t, "test failed", "warm hit waited on the repository gate while another holder kept it")
	}
}

func TestResponseFileCloseReleasesScopePin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pack")
	require.NoError(t, os.WriteFile(path, []byte("response"), 0600))
	file, err := os.Open(path)
	require.NoError(t, err)
	pins := 0
	f := &responseFile{file: file, budget: &responseBudget{limit: 1 << 20}, limit: 1 << 20, persist: true, unpin: func() { pins++ }}
	require.NoError(t, f.Close())
	require.NoError(t, f.Close())
	require.EqualValues(t, 1, pins, "pin released %d times", pins)
}
