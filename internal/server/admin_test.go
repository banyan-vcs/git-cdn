package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"gitlab.com/banyan-vcs/git-cdn/internal/buildinfo"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/testlog"
)

func adminJSON(t *testing.T, method, url string) (int, map[string]any) {
	t.Helper()
	r, err := http.NewRequest(method, url, nil)
	require.NoError(t, err)
	response, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	t.Cleanup(func() { response.Body.Close() })
	var body map[string]any
	require.NoError(t, json.NewDecoder(response.Body).Decode(&body))
	return response.StatusCode, body
}

func changeFields(list any) []string {
	entries, _ := list.([]any)
	fields := make([]string, 0, len(entries))
	for _, entry := range entries {
		if change, ok := entry.(map[string]any); ok {
			fields = append(fields, change["field"].(string))
		}
	}
	return fields
}

func TestAdminStatusAndScopesReportLiveState(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	admin := httptest.NewServer(s.AdminHandler())
	t.Cleanup(admin.Close)

	// One local fetch seeds a scope so the reported numbers are not all zero.
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, "MISS", r.Header.Get("X-GitCDN-Cache-Status"))
	responseBody(t, r)

	status, body := adminJSON(t, http.MethodGet, admin.URL+"/status")
	require.EqualValues(t, http.StatusOK, status)
	require.Equal(t, "ok", body["status"])
	require.Equal(t, buildinfo.Version, body["version"])
	require.Equal(t, "multi", body["mode"])
	require.EqualValues(t, 0, body["active_ssh_sessions"])
	cache, ok := body["cache"].(map[string]any)
	require.True(t, ok, "cache section missing: %v", body)
	require.EqualValues(t, 1, cache["entries"])
	require.Positive(t, cache["bytes"])
	require.Positive(t, cache["max_bytes"])
	require.NotNil(t, body["disk_free_bytes"])
	totals, ok := body["totals"].(map[string]any)
	require.True(t, ok, "totals section missing: %v", body)
	require.EqualValues(t, 1, totals["local_requests"])

	// Scopes list the cached repository under its site, keyed by the
	// directory hash and never by a repository name.
	_, body = adminJSON(t, http.MethodGet, admin.URL+"/scopes")
	scopes, ok := body["scopes"].([]any)
	require.True(t, ok, "scopes list missing: %v", body)
	require.Len(t, scopes, 1)
	one, ok := scopes[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "gitlab", one["site"])
	require.Regexp(t, "^[0-9a-f]{64}$", one["scope"])
	require.Positive(t, one["bytes"])
	require.NotEmpty(t, one["last_access"])
}

func TestDrainStopsAdmissionOnBothEndsOfHealthz(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	require.False(t, s.Draining())
	admin := httptest.NewServer(s.AdminHandler())
	t.Cleanup(admin.Close)

	status, body := adminJSON(t, http.MethodPost, admin.URL+"/drain")
	require.EqualValues(t, http.StatusOK, status)
	require.Equal(t, "draining", body["status"])
	require.True(t, s.Draining())

	// The health probe reports the drain so load balancers stop routing
	// here, and new git work is refused before admission.
	health, err := http.Get(h.URL + "/healthz")
	require.NoError(t, err)
	health.Body.Close()
	require.EqualValues(t, http.StatusServiceUnavailable, health.StatusCode)

	git, err := http.Get(h.URL + "/gitlab/Group/Repo.git/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	git.Body.Close()
	require.EqualValues(t, http.StatusServiceUnavailable, git.StatusCode)

	// Metrics keep answering through a drain; operators watch them while
	// the node empties.
	metrics, err := http.Get(h.URL + "/metrics")
	require.NoError(t, err)
	metrics.Body.Close()
	require.EqualValues(t, http.StatusOK, metrics.StatusCode)

	status, body = adminJSON(t, http.MethodPost, admin.URL+"/undrain")
	require.EqualValues(t, http.StatusOK, status)
	require.Equal(t, "ok", body["status"])
	require.False(t, s.Draining())
	health, err = http.Get(h.URL + "/healthz")
	require.NoError(t, err)
	health.Body.Close()
	require.EqualValues(t, http.StatusOK, health.StatusCode)
}

func TestReloadAppliesLimitsAndReportsRestartFields(t *testing.T) {
	f := newFixture(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	write := func(maxCache int64, listen string) {
		body := fmt.Sprintf("mode: multi\nlisten: %s\ncache_dir: %q\nmax_cache_bytes: %d\nupstreams:\n  gitlab:\n    url: %q\n",
			listen, filepath.Join(dir, "caches"), maxCache, f.origin.URL+"/root")
		require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	}
	write(1<<30, "127.0.0.1:0")
	c, err := config.Load(path)
	require.NoError(t, err)
	s, err := New(c, testlog.New(t))
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	s.SetConfigPath(path)
	admin := httptest.NewServer(s.AdminHandler())
	t.Cleanup(admin.Close)

	// One edit that moves a reloadable field and a restart field applies
	// the former and reports the latter by name.
	write(512<<20, "127.0.0.1:18085")
	status, body := adminJSON(t, http.MethodPost, admin.URL+"/reload")
	require.EqualValues(t, http.StatusOK, status)
	require.EqualValues(t, 512<<20, s.store.Stats().MaxBytes)
	require.Contains(t, changeFields(body["applied"]), "max_cache_bytes")
	require.Contains(t, changeFields(body["restart_required"]), "listen")

	// Reloading the same file again observes no new change: applied is
	// empty, and the restart list keeps reporting the pending difference
	// between the running node and the file until a restart.
	status, body = adminJSON(t, http.MethodPost, admin.URL+"/reload")
	require.EqualValues(t, http.StatusOK, status)
	require.Empty(t, changeFields(body["applied"]))
	require.Equal(t, []string{"listen"}, changeFields(body["restart_required"]))

	// A file that no longer validates is refused whole; the running limits
	// are untouched.
	require.NoError(t, os.WriteFile(path, []byte("listen: 127.0.0.1:0\nbogus_field: yes\n"), 0600))
	status, body = adminJSON(t, http.MethodPost, admin.URL+"/reload")
	require.EqualValues(t, http.StatusBadRequest, status)
	require.Contains(t, fmt.Sprint(body["error"]), "bogus_field")
	require.EqualValues(t, 512<<20, s.store.Stats().MaxBytes)
}
