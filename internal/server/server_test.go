package server

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/chiyutianyi/git-cdn/internal/nativegit"

	"github.com/stretchr/testify/require"
)

func TestCloneCacheAndIncrementalFetch(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	cloneURL := h.URL + "/gitlab/Group/Repo.git"
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	testGit(t, "", "clone", "--quiet", cloneURL, first)
	{
		tip := testGit(t, first, "rev-parse", "HEAD")
		require.EqualValues(t, f.tip, tip, "tip %s != %s", tip, f.tip)
	}
	require.EqualValues(t, "v1", testGit(t, first, "tag"), "annotated tag lost")
	firstFetches, firstProbes := f.fetches.Load(), f.probes.Load()
	require.NotEqualValues(t, 0, firstFetches, "clone did not hydrate and serve locally: fetches=%d local=%d", firstFetches, s.local.Load())
	require.NotEqualValues(t, 0, s.local.Load(), "clone did not hydrate and serve locally: fetches=%d local=%d", firstFetches, s.local.Load())
	testGit(t, "", "clone", "--quiet", cloneURL, second)
	require.EqualValues(t, firstFetches, f.fetches.Load(), "hot clone fetched object data from upstream")
	require.False(t, f.probes.Load() <= firstProbes, "hot clone skipped live refs authorization")
	f.commit(t, "updated")
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	testGit(t, first, "fetch", "--quiet", "origin")
	require.EqualValues(t, f.tip, testGit(t, first, "rev-parse", "origin/main"), "incremental fetch did not discover updated tip")
	require.False(t, f.fetches.Load() <= firstFetches, "new tip was not hydrated")
	require.False(t, s.local.Load() < 3, "incremental fetch bypassed local upload-pack")
}

func TestDirectFetchRechecksAuthorizationOnCacheHit(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	for _, expected := range []string{"MISS", "HIT"} {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
		require.EqualValues(t, expected, r.Header.Get("X-GitCDN-Cache-Status"))
		require.True(t, bytes.Contains(responseBody(t, r), []byte("packfile\n")), "missing pack section")
	}
	localBefore := s.local.Load()
	f.denied.Store(true)
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	denied, _ := io.ReadAll(r.Body)
	require.EqualValues(t, 401, r.StatusCode, "revoked authorization served cached data: %d %s", r.StatusCode, denied)
	require.EqualValues(t, `Basic realm="Origin"`, r.Header.Get("WWW-Authenticate"), "revoked authorization served cached data: %d %s", r.StatusCode, denied)
	require.False(t, bytes.Contains(denied, []byte("PACK")), "revoked authorization served cached data: %d %s", r.StatusCode, denied)
	require.EqualValues(t, localBefore, s.local.Load(), "revoked authorization served cached data: %d %s", r.StatusCode, denied)
}

func TestCredentialsShareObjectsAndKeepAuthorizationIndependent(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	for i, authorization := range []string{testAuthorization, "Bearer second-user"} {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), authorization, nil)
		expected := "HIT"
		if i == 0 {
			expected = "MISS"
		}
		require.Equal(t, expected, r.Header.Get("X-GitCDN-Cache-Status"))
		responseBody(t, r)
	}
	require.EqualValues(t, 1, s.hydrated.Load())
	require.EqualValues(t, 1, s.store.Stats().Entries)
	require.EqualValues(t, 2, f.probes.Load())
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), "Bearer denied-user", nil)
	require.Equal(t, http.StatusUnauthorized, r.StatusCode)
	r.Body.Close()
	require.EqualValues(t, 2, s.local.Load())
}

func TestDeletedRefOIDGoesToOrigin(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	old := f.tip
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(old), testAuthorization, nil))
	testGit(t, f.work, "checkout", "--quiet", "--orphan", "replacement")
	testGit(t, f.work, "rm", "-rf", "--quiet", ".")
	f.commit(t, "replacement")
	testGit(t, f.work, "push", "--quiet", "--force", f.repo, "HEAD:main")
	testGit(t, f.repo, "update-ref", "-d", "refs/tags/v1")
	before := s.local.Load()
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(old), testAuthorization, nil)
	require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"), "stale cached OID served locally")
	require.EqualValues(t, before, s.local.Load(), "stale cached OID served locally")
	responseBody(t, r)
}

func TestUnsupportedRequestsAndPushAreProxied(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	for _, args := range [][]string{{"deepen 1"}, {"filter blob:none"}} {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, args...), testAuthorization, nil)
		require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"), "unsupported request entered local path")
		responseBody(t, r)
	}
	clone := filepath.Join(t.TempDir(), "client")
	testGit(t, "", "clone", "--quiet", h.URL+"/gitlab/Group/Repo.git", clone)
	require.NoError(t, os.WriteFile(filepath.Join(clone, "pushed.txt"), []byte("push data\n"), 0600))
	testGit(t, clone, "add", "pushed.txt")
	testGit(t, clone, "commit", "--quiet", "-m", "through CDN")
	testGit(t, clone, "push", "--quiet", "origin", "main")
	require.EqualValues(t, "push data", testGit(t, f.repo, "show", "main:pushed.txt"), "push did not reach origin")
	require.NotEqualValues(t, 0, s.local.Load(), "ordinary clone did not use cache")
}

func TestCorruptCacheRepairsFromOrigin(t *testing.T) {
	f := newFixture(t)
	// The response pack cache would short-circuit identical requests; this
	// test exercises object-store repair, so it runs with the pack cache off.
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxPackCacheBytes = -1 })
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	var removed int
	err := filepath.WalkDir(filepath.Join(s.store.Root, "repos"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".pack") {
			removed++
			return os.Remove(path)
		}
		return nil
	})
	require.NoError(t, err, "cannot remove cached pack: %v", err)
	require.NotEqualValues(t, 0, removed, "cannot remove cached pack: %v", err)
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, "MISS", r.Header.Get("X-GitCDN-Cache-Status"), "cache repair failed: %v", r.Header)
	responseBody(t, r)
}

func TestLocalResponseFailureFallsBackBeforeHeaders(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxResponseBytes = 16 })
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"), "failed local pack was committed")
	body := responseBody(t, r)
	require.EqualValues(t, 1, bytes.Count(body, []byte("packfile\n")), "fallback appended a second response")
	require.EqualValues(t, 0, s.local.Load(), "fallback appended a second response")
}

func TestConcurrentMissHasOneHydration(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
			responseBody(t, r)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, s.hydrated.Load(), "concurrent miss: hydration=%d local=%d", s.hydrated.Load(), s.local.Load())
	require.EqualValues(t, 6, s.local.Load(), "concurrent miss: hydration=%d local=%d", s.hydrated.Load(), s.local.Load())
}

func TestInvalidProbeCannotServeCache(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<html>sign in</html>")
	}))
	defer bad.Close()
	u, _ := url.Parse(bad.URL)
	s.upstreams["gitlab"] = u
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, 502, r.StatusCode, "HTML 200 accepted as Git authorization")
}

func TestNoCredentialsInPersistentCache(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	decoded, _ := base64.StdEncoding.DecodeString(strings.TrimPrefix(testAuthorization, "Basic "))
	err := filepath.WalkDir(s.store.Root, func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(b, []byte(testAuthorization)) || bytes.Contains(b, decoded) {
			return fmt.Errorf("credentials persisted in %s", path)
		}
		return nil
	})
	require.NoError(t, err)
}

func TestMissingBlobIsRepairedWhenCommitStillExists(t *testing.T) {
	f := newFixture(t)
	// Runs with the response pack cache off; see TestCorruptCacheRepairsFromOrigin.
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxPackCacheBytes = -1 })
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	var repo string
	var pack []byte
	err := filepath.WalkDir(filepath.Join(s.store.Root, "repos"), func(path string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(path, ".pack") {
			repo = filepath.Dir(filepath.Dir(filepath.Dir(path)))
			pack, err = os.ReadFile(path)
			if err != nil {
				return err
			}
			if err = os.Remove(path); err != nil {
				return err
			}
			return os.Remove(strings.TrimSuffix(path, ".pack") + ".idx")
		}
		return nil
	})
	require.NoError(t, err)
	require.NotEqualValues(t, 0, len(pack), "no cached pack")
	cmd := exec.Command("git", "unpack-objects", "-r")
	cmd.Dir = repo
	cmd.Env = nativegit.Environment(nil)
	cmd.Stdin = bytes.NewReader(pack)
	{
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "unpack: %v %s", err, out)
	}
	blob := testGit(t, repo, "rev-parse", f.tip+":file.txt")
	require.NoError(t, os.Remove(filepath.Join(repo, "objects", blob[:2], blob[2:])))
	require.EqualValues(t, "commit", testGit(t, repo, "cat-file", "-t", f.tip), "wanted commit unexpectedly missing")
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, "MISS", r.Header.Get("X-GitCDN-Cache-Status"), "missing blob was not repaired: %v", r.Header)
	responseBody(t, r)
}

func TestRepositoryIsolationWithIdenticalObjects(t *testing.T) {
	f := newFixture(t)
	other := filepath.Join(filepath.Dir(filepath.Dir(f.repo)), "Other", "Repo.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(other), 0700))
	testGit(t, "", "clone", "--quiet", "--bare", f.repo, other)
	s, h := newCDN(t, f, nil)
	for _, path := range []string{"Group/Repo.git", "Other/Repo.git"} {
		r := postFetch(t, h.URL, "/gitlab/"+path+"/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
		require.EqualValues(t, "MISS", r.Header.Get("X-GitCDN-Cache-Status"), "different repositories shared cache")
		responseBody(t, r)
	}
	require.EqualValues(t, 2, s.hydrated.Load(), "repository identity not isolated")
}

func TestHydrationTimeoutFallsBack(t *testing.T) {
	f := newFixture(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && strings.HasPrefix(r.Header.Get("User-Agent"), "git/") {
			<-r.Context().Done()
			return
		}
		f.origin.Config.Handler.ServeHTTP(w, r)
	}))
	defer origin.Close()
	s, h := newCDN(t, f, func(c *config.Config) {
		c.FetchTimeout = "200ms"
		c.Upstreams["gitlab"] = config.Upstream{URL: origin.URL + "/root"}
	})
	started := time.Now()
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"), "hydration timeout did not promptly fall back")
	require.False(t, time.Since(started) > 3*time.Second, "hydration timeout did not promptly fall back")
	responseBody(t, r)
	entries, err := os.ReadDir(s.store.Tmp)
	require.NoError(t, err, "failed hydration retained staging data: %v %v", entries, err)
	require.Len(t, entries, 0, "failed hydration retained staging data: %v %v", entries, err)
}

func TestMalformedAndOversizedFetchCannotStartGit(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxRequestBytes = 128 })
	for _, test := range []struct {
		body   []byte
		status int
	}{{[]byte("ffff"), 400}, {bytes.Repeat([]byte("x"), 129), 413}} {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", test.body, testAuthorization, nil)
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
		require.EqualValues(t, test.status, r.StatusCode, "status=%d want=%d", r.StatusCode, test.status)
	}
	require.EqualValues(t, 0, f.probes.Load(), "malformed input started upstream/local Git processing")
	require.EqualValues(t, 0, s.local.Load(), "malformed input started upstream/local Git processing")
}

func TestCacheDirectoryCannotBeSharedByProcesses(t *testing.T) {
	f := newFixture(t)
	s, _ := newCDN(t, f, nil)
	_, err := New(s.config, nil)
	require.Error(t, err, "second server acquired an active cache directory")
}

func TestRepositoryLRUEvictsAndRefetches(t *testing.T) {
	f := newFixture(t)
	for _, name := range []string{"Other", "Third"} {
		path := filepath.Join(filepath.Dir(filepath.Dir(f.repo)), name, "Repo.git")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		testGit(t, "", "clone", "--quiet", "--bare", f.repo, path)
	}
	s, h := newCDN(t, f, nil)
	request := func(group, expected string) {
		t.Helper()
		r := postFetch(t, h.URL, "/gitlab/"+group+"/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
		require.EqualValues(t, expected, r.Header.Get("X-GitCDN-Cache-Status"), "%s expected %s: %v", group, expected, r.Header)
		responseBody(t, r)
	}
	request("Group", "MISS")
	firstSize := s.store.Stats().Bytes
	// Measure the fixture first, then use a separate store that fits two scopes.
	s, h = newCDN(t, f, func(c *config.Config) { c.MaxCacheBytes = firstSize*2 + 4096 })
	request("Group", "MISS")
	request("Other", "MISS")
	request("Group", "HIT")
	request("Third", "MISS")
	stats := s.store.Stats()
	require.EqualValues(t, 1, stats.Evictions, "LRU did not stay within limit: %+v", stats)
	require.False(t, stats.Bytes > stats.MaxBytes, "LRU did not stay within limit: %+v", stats)
	request("Other", "MISS")
	request("Other", "HIT")
	require.EqualValues(t, 2, s.store.Stats().Evictions, "evicted repository was not fetched again")
}

func TestOversizedRepositoryFallsBackWithoutPublishing(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxCacheBytes = 1 })
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"), "oversized repository did not fall back")
	require.True(t, bytes.Contains(responseBody(t, r), []byte("packfile\n")), "fallback did not return a valid pack")
	require.EqualValues(t, 0, s.local.Load(), "oversized repository entered persistent cache")
	require.EqualValues(t, 0, s.store.Stats().Bytes, "oversized repository entered persistent cache")
	require.EqualValues(t, 0, s.store.Stats().ReservedBytes, "oversized repository entered persistent cache")
}
