package server

import (
	"io"
	"sync"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/config"

	"github.com/stretchr/testify/require"
)

// Concurrent fetches with the same credential merge into a single live
// authorization probe; no configuration is required and semantics stay
// live because waiters reuse the leader's fresh result. The origin delay
// widens the in-flight window so slow runners cannot straddle it.
func TestConcurrentFetchesMergeAuthorizationProbes(t *testing.T) {
	f := newFixture(t)
	f.delay.Store(int64(300 * time.Millisecond))
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	const requests = 6
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			require.NotEmpty(t, responseBody(t, r))
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, f.probes.Load(), "concurrent identical fetches probed %d times", f.probes.Load())
	require.EqualValues(t, requests, s.local.Load())
	require.EqualValues(t, requests-1, s.probeMerged.Load())
	require.EqualValues(t, requests-1, metricValue(t, h, "gitcdn_auth_probes_merged_total"))
}

func TestAuthProbeCacheReusesSnapshotsWithinTTL(t *testing.T) {
	f := newFixture(t)
	_, h := newCDN(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "10s" })
	body := fetchBody(f.tip)
	r1 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r1)
	require.EqualValues(t, "LIVE", r1.Header.Get("X-GitCDN-Auth-Cache"))
	r2 := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	responseBody(t, r2)
	require.EqualValues(t, "HIT", r2.Header.Get("X-GitCDN-Auth-Cache"))
	require.EqualValues(t, 1, f.probes.Load(), "a successful probe was reused within the TTL")
	require.EqualValues(t, 1, metricValue(t, h, "gitcdn_auth_probe_cache_hits_total"))
}

func TestAuthProbeCacheExpires(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "10s" })
	body := fetchBody(f.tip)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	require.EqualValues(t, 1, f.probes.Load())
	s.probes.mu.Lock()
	for key, entry := range s.probes.entries {
		entry.expires = time.Now().Add(-time.Second)
		s.probes.entries[key] = entry
	}
	s.probes.mu.Unlock()
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	require.EqualValues(t, 2, f.probes.Load(), "an expired probe was served from the cache")
}

// Denials are never reused: once the origin rejects a credential, every
// request re-probes so a corrected or replaced credential takes effect
// immediately.
func TestAuthProbeCacheIgnoresDenials(t *testing.T) {
	f := newFixture(t)
	f.denied.Store(true)
	_, h := newCDN(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "10s" })
	body := fetchBody(f.tip)
	for range 2 {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
		_, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		r.Body.Close()
		require.EqualValues(t, 401, r.StatusCode)
	}
	require.EqualValues(t, 2, f.attempts.Load(), "denials were cached or merged away")
}

func TestAuthProbeCacheSeparatesCredentials(t *testing.T) {
	f := newFixture(t)
	_, h := newCDN(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "10s" })
	body := fetchBody(f.tip)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, "Bearer second-user", nil))
	require.EqualValues(t, 2, f.probes.Load(), "credentials share one probe entry")
}

// The documented tradeoff, pinned deliberately: within the TTL a revoked
// credential still reads cached data. Operators choose the window.
func TestAuthProbeCacheRevocationWindow(t *testing.T) {
	f := newFixture(t)
	_, h := newCDN(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "10s" })
	body := fetchBody(f.tip)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	f.denied.Store(true)
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	require.EqualValues(t, 200, r.StatusCode, "near-live authorization must be an explicit configured choice")
	require.EqualValues(t, "HIT", r.Header.Get("X-GitCDN-Auth-Cache"))
}
