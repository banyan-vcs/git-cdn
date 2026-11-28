package server

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"

	"github.com/stretchr/testify/require"
)

func metricsText(t *testing.T, h *httptest.Server) string {
	t.Helper()
	r, err := http.Get(h.URL + "/metrics")
	require.NoError(t, err)
	defer r.Body.Close()
	require.Equal(t, "text/plain; version=0.0.4", r.Header.Get("Content-Type"))
	data, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	return string(data)
}

// metricsOf parses /metrics into samples named with their labels.
func metricsOf(t *testing.T, h *httptest.Server) map[string]float64 {
	t.Helper()
	values := make(map[string]float64)
	scanner := bufio.NewScanner(strings.NewReader(metricsText(t, h)))
	for scanner.Scan() {
		name, sample, ok := strings.Cut(scanner.Text(), " ")
		if !ok {
			continue
		}
		value, err := strconv.ParseFloat(sample, 64)
		require.NoError(t, err, "metric %s", name)
		values[name] = value
	}
	require.NoError(t, scanner.Err())
	return values
}

// /metrics describes the repositories in aggregate — how many, how idle,
// how large, how they split over sites and how concentrated the requests
// are — without naming one. A second site fronts a second origin here, so
// that the per-site split shows.
func TestMetricsDescribeRepositoriesWithoutNamingThem(t *testing.T) {
	f, g := newFixture(t), newFixture(t)
	_, h := newCDN(t, f, func(c *config.Config) { c.Upstreams["github"] = config.Upstream{URL: g.origin.URL + "/root"} })
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	for range 2 {
		responseBody(t, postFetch(t, h.URL, "/github/Group/Repo.git/git-upload-pack", fetchBody(g.tip), testAuthorization, nil))
	}
	before := time.Now()
	output := metricsText(t, h)
	for _, name := range []string{"Group", "Repo.git", "refs/"} {
		require.NotContains(t, output, name, "the metrics name a repository")
	}
	values := metricsOf(t, h)
	for name, want := range map[string]float64{
		"gitcdn_repositories":                                       2,
		`gitcdn_site_repositories{site="gitlab"}`:                   1,
		`gitcdn_site_repositories{site="github"}`:                   1,
		`gitcdn_site_requests_total{site="gitlab"}`:                 1,
		`gitcdn_site_requests_total{site="github"}`:                 2,
		`gitcdn_repository_idle_bucket{min_idle_seconds="3600"}`:    0,
		`gitcdn_repository_idle_bucket{min_idle_seconds="2592000"}`: 0,
		`gitcdn_repository_size_bucket{min_bytes="1048576"}`:        0,
		`gitcdn_active_repositories{window="1h"}`:                   2,
		`gitcdn_active_repositories{window="7d"}`:                   2,
		"gitcdn_request_top10_share":                                1,
		"gitcdn_hydration_backoff_repositories":                     0,
		"gitcdn_maintenance_backoff_repositories":                   0,
	} {
		require.Contains(t, values, name)
		require.Equal(t, want, values[name], name)
	}
	for _, name := range []string{
		"gitcdn_repository_bytes_largest", `gitcdn_repository_bytes_quantile{quantile="0.5"}`, `gitcdn_repository_bytes_quantile{quantile="0.9"}`,
		"gitcdn_pack_cache_bytes", "gitcdn_disk_free_bytes",
	} {
		require.Contains(t, values, name)
		require.Positive(t, values[name], name)
	}
	for _, threshold := range cache.IdleThresholds {
		seconds := strconv.FormatInt(int64(threshold/time.Second), 10)
		require.Contains(t, values, `gitcdn_repository_idle_bucket{min_idle_seconds="`+seconds+`"}`)
		require.Contains(t, values, `gitcdn_repository_idle_bucket_bytes{min_idle_seconds="`+seconds+`"}`)
	}
	for _, threshold := range cache.SizeThresholds {
		bytes := strconv.FormatInt(threshold, 10)
		require.Contains(t, values, `gitcdn_repository_size_bucket{min_bytes="`+bytes+`"}`)
		require.Contains(t, values, `gitcdn_repository_size_bucket_bytes{min_bytes="`+bytes+`"}`)
	}
	for _, window := range []string{"1h", "24h", "7d"} {
		require.Contains(t, values, `gitcdn_active_repositories{window="`+window+`"}`)
	}
	started := time.Unix(int64(values["gitcdn_process_start_time_seconds"]), 0)
	require.WithinDuration(t, before, started, time.Minute)
}
