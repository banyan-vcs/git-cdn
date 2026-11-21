package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"sync"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/chiyutianyi/git-cdn/internal/protocol"

	"github.com/stretchr/testify/require"
)

// A failing upstream relayed to merged waiters must not wedge the flight:
// every concurrent request sees the upstream outcome, and a later request
// against a recovered upstream succeeds.
func TestProbeFailuresPropagateToWaitersAndRecover(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxPackCacheBytes = -1 })
	original := s.upstreams["gitlab"]
	body := fetchBody(f.tip)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)
	broken, err := url.Parse(bad.URL)
	require.NoError(t, err)
	s.upstreams["gitlab"] = broken
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			payload, readErr := io.ReadAll(r.Body)
			require.NoError(t, readErr)
			require.EqualValues(t, 500, r.StatusCode, "relayed: %s", payload)
		}()
	}
	wg.Wait()
	s.upstreams["gitlab"] = original
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
}

// A waiter whose leader disconnects mid-probe still receives the shared
// probe's result: the probe runs detached from the leader's connection.
func TestProbeOutlivesLeaderDisconnection(t *testing.T) {
	f := newFixture(t)
	f.delay.Store(int64(600 * time.Millisecond))
	_, h := newCDN(t, f, func(c *config.Config) { c.MaxPackCacheBytes = -1 })
	body := fetchBody(f.tip)
	path := "/gitlab/Group/Repo.git/git-upload-pack"
	request, err := http.NewRequest("POST", h.URL+path, bytes.NewReader(body))
	require.NoError(t, err)
	request.Header.Set("Authorization", testAuthorization)
	request.Header.Set("Git-Protocol", "version=2")
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	abandoner := &http.Client{Timeout: 150 * time.Millisecond}
	leader, err := abandoner.Do(request)
	if err == nil {
		leader.Body.Close()
	}
	require.Error(t, err, "the abandoning client unexpectedly completed")
	// The probe started by the abandoned request is still in flight; this
	// request joins it and must receive its result, not the disconnect.
	responseBody(t, postFetch(t, h.URL, path, body, testAuthorization, nil))
}

func TestProbeKeySeparatesDimensions(t *testing.T) {
	s := &Server{}
	base, err := url.Parse("https://git.example.com")
	require.NoError(t, err)
	other, err := url.Parse("https://mirror.example.com")
	require.NoError(t, err)
	rt := route{id: "gitlab", base: base, repo: "Group/Repo.git"}
	headers := [][2]string{{"Authorization", "token-a"}}
	require.Equal(t, s.probeKey(rt, headers), s.probeKey(rt, headers))
	require.NotEqual(t, s.probeKey(rt, headers), s.probeKey(route{id: "gitlab", base: base, repo: "Other/Repo.git"}, headers))
	require.NotEqual(t, s.probeKey(rt, headers), s.probeKey(route{id: "gitlab", base: other, repo: "Group/Repo.git"}, headers))
	require.NotEqual(t, s.probeKey(rt, headers), s.probeKey(rt, [][2]string{{"Authorization", "token-b"}}))
}

func TestProbeCacheEntryBound(t *testing.T) {
	s := &Server{}
	s.probes.init(time.Minute)
	for i := range probeCacheLimit {
		s.probes.entries[fmt.Sprintf("k%06d", i)] = probeEntry{expires: time.Now().Add(time.Hour)}
	}
	s.rememberProbe("fresh-key", protocol.Snapshot{})
	s.probes.mu.Lock()
	size := len(s.probes.entries)
	s.probes.mu.Unlock()
	require.LessOrEqual(t, size, probeCacheLimit, "entry bound exceeded: %d", size)
}

// The tag validation budget applies on both serving paths: an over-budget
// include-tag request falls back upstream instead of validating locally.
func TestIncludeTagBudgetFallsBackUpstream(t *testing.T) {
	f := newFixture(t)
	var updates bytes.Buffer
	for i := range 130 {
		fmt.Fprintf(&updates, "create refs/tags/bulk-%03d %s\n", i, f.tip)
	}
	cmd := exec.Command("git", "-C", f.repo, "update-ref", "--stdin")
	cmd.Stdin = &updates
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bulk tags: %s", out)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip, "include-tag")
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	payload := responseBody(t, r)
	require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"), "over-budget tags were validated locally")
	require.EqualValues(t, 0, s.local.Load())
	require.NotEmpty(t, payload)
}
