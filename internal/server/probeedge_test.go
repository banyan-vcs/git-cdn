package server

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"

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

// A repository with many tags keeps fetching from the cache. The tags an
// include-tag response may carry are decided in one batch with no cap on
// their number, and annotated tags that sit behind the want become roots of
// their own. An ordinary `git fetch` after the upstream moves — one new
// commit wanted, the client holding the history, include-tag set — is
// served locally end to end.
func TestManyTagsStayOnLocalPath(t *testing.T) {
	f := newFixture(t)
	first := f.tip
	f.commit(t, "second")
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	var updates bytes.Buffer
	for i := range 130 {
		fmt.Fprintf(&updates, "create refs/tags/bulk-%03d %s\n", i, first)
	}
	cmd := exec.Command("git", "-C", f.repo, "update-ref", "--stdin")
	cmd.Stdin = &updates
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bulk tags: %s", out)
	for i := range 3 {
		testGit(t, f.work, "tag", "-a", "-m", "annotated", fmt.Sprintf("annotated-%d", i), first)
	}
	testGit(t, f.work, "push", "--quiet", f.repo, "--tags")
	s, h := newCDN(t, f, nil)
	client := filepath.Join(t.TempDir(), "client")
	cloneURL := h.URL + "/gitlab/Group/Repo.git"
	testGit(t, "", "clone", "--quiet", "--single-branch", "--no-tags", cloneURL, client)
	require.EqualValues(t, 1, s.local.Load(), "the clone of a many-tag repository bypassed the cache")
	// The clone's include-tag response already carries the tag objects;
	// a real fetch needs something new from the upstream.
	f.commit(t, "third")
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	local, proxied := s.local.Load(), s.proxied.Load()
	testGit(t, client, "fetch", "--quiet", "--tags", "origin")
	// v1, 130 lightweight and 3 annotated tags all point into history the client already has.
	require.Len(t, strings.Fields(testGit(t, client, "tag")), 134, "tags were not fetched")
	require.Equal(t, f.tip, testGit(t, client, "rev-parse", "origin/main"), "fetch did not reach the new tip")
	// Discovery and ls-refs pass through; the fetch itself is served locally.
	require.Equal(t, local+1, s.local.Load(), "the tag fetch bypassed the cache")
	require.Equal(t, proxied+2, s.proxied.Load(), "the fetch request itself was forwarded")
}
