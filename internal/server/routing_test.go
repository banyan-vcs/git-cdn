package server

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/chiyutianyi/git-cdn/internal/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGzipPassthroughPreservesBodyAndRoute(t *testing.T) {
	var compressed bytes.Buffer
	g := gzip.NewWriter(&compressed)
	g.Write(fetchBody(strings.Repeat("a", 40), "filter blob:none"))
	g.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		assert.False(t, r.URL.Path != "/base/Group/Repo.git/git-upload-pack" || r.URL.RawQuery != "custom=1" || r.Header.Get("Content-Encoding") != "gzip" || !bytes.Equal(body, compressed.Bytes()), "passthrough altered body, case or query")
		assert.False(t, r.Header.Get("X-Forwarded-User") != "" || r.Header.Get("Proxy-Authorization") != "", "untrusted identity header forwarded")
		w.Write([]byte("origin response"))
	}))
	defer origin.Close()
	f := &fixture{origin: origin}
	_, h := newCDN(t, f, func(c *config.Config) { c.Upstreams["gitlab"] = config.Upstream{URL: origin.URL + "/base"} })
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack?custom=1", compressed.Bytes(), testAuthorization, http.Header{"Content-Encoding": {"gzip"}, "X-Forwarded-User": {"administrator"}, "Proxy-Authorization": {"secret"}})
	require.EqualValues(t, "origin response", string(responseBody(t, r)), "passthrough response altered")
}

func TestCanonicalRedirectStaysOnCDN(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/base/Group/Renamed.git/info/refs?service=git-upload-pack")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer origin.Close()
	f := &fixture{origin: origin}
	_, h := newCDN(t, f, func(c *config.Config) { c.Upstreams["gitlab"] = config.Upstream{URL: origin.URL + "/base"} })
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Get(h.URL + "/gitlab/Group/Repo.git/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	defer r.Body.Close()
	require.EqualValues(t, "/gitlab/Group/Renamed.git/info/refs?service=git-upload-pack", r.Header.Get("Location"), "redirect escaped CDN: %s", r.Header.Get("Location"))
}

func TestRepositoryURLAliasesCloneFetchPushAndShareCache(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeSingle, config.ModeMulti} {
		for _, firstSuffix := range []string{"", ".git"} {
			t.Run(string(mode)+"_first_"+firstSuffix, func(t *testing.T) {
				f := newFixture(t)
				s, h := newCDN(t, f, func(c *config.Config) {
					if mode == config.ModeSingle {
						u := c.Upstreams["gitlab"]
						c.Mode, c.Upstream, c.Upstreams = mode, &u, nil
					}
				})
				prefix := ""
				if mode == config.ModeMulti {
					prefix = "/gitlab"
				}
				base := h.URL + prefix + "/Group/Repo"
				first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
				testGit(t, "", "clone", "--quiet", base+firstSuffix, first)
				fetches := f.fetches.Load()
				secondSuffix := ".git"
				if firstSuffix == ".git" {
					secondSuffix = ""
				}
				testGit(t, "", "clone", "--quiet", base+secondSuffix, second)
				require.EqualValues(t, fetches, f.fetches.Load(), "URL aliases used different object caches")
				require.EqualValues(t, 2, s.local.Load(), "alias clone did not serve correct local objects")
				require.EqualValues(t, f.tip, testGit(t, first, "rev-parse", "HEAD"), "alias clone did not serve correct local objects")
				require.EqualValues(t, f.tip, testGit(t, second, "rev-parse", "HEAD"), "alias clone did not serve correct local objects")
				require.NoError(t, os.WriteFile(filepath.Join(first, "alias.txt"), []byte("alias push\n"), 0600))
				testGit(t, first, "add", "alias.txt")
				testGit(t, first, "commit", "--quiet", "-m", "alias push")
				testGit(t, first, "push", "--quiet", "origin", "main")
				testGit(t, second, "fetch", "--quiet", "origin")
				require.EqualValues(t, "alias push", testGit(t, second, "show", "origin/main:alias.txt"), "alias push/fetch failed")
				require.EqualValues(t, 3, s.local.Load(), "incremental alias fetch did not serve locally")
				probes := f.probes.Load()
				tip := testGit(t, first, "rev-parse", "HEAD")
				for _, suffix := range []string{"", ".git"} {
					r := postFetch(t, h.URL, prefix+"/Group/Repo"+suffix+"/git-upload-pack", fetchBody(tip), testAuthorization, nil)
					require.EqualValues(t, "HIT", r.Header.Get("X-GitCDN-Cache-Status"), "direct alias fetch missed cache: %v", r.Header)
					responseBody(t, r)
				}
				require.EqualValues(t, probes+2, f.probes.Load(), "URL alias skipped per-request authorization")
			})
		}
	}
}

func TestRepositoryURLAliasesForV0AndLFS(t *testing.T) {
	for _, mode := range []config.Mode{config.ModeSingle, config.ModeMulti} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t)
			s, h := newCDN(t, f, func(c *config.Config) {
				if mode == config.ModeSingle {
					u := c.Upstreams["gitlab"]
					c.Mode, c.Upstream, c.Upstreams = mode, &u, nil
				}
			})
			prefix := ""
			if mode == config.ModeMulti {
				prefix = "/gitlab"
			}
			clone := filepath.Join(t.TempDir(), "v0")
			testGit(t, "", "-c", "protocol.version=0", "clone", "--quiet", h.URL+prefix+"/Group/Repo", clone)
			require.EqualValues(t, f.tip, testGit(t, clone, "rev-parse", "HEAD"), "v0 alias clone was not transparently proxied")
			require.EqualValues(t, 0, s.local.Load(), "v0 alias clone was not transparently proxied")
			for _, path := range []string{prefix + "/Group/Repo", prefix + "/Group/Repo.git"} {
				r, err := http.NewRequest("POST", h.URL+path+"/info/lfs/objects/batch", strings.NewReader(`{"operation":"download"}`))
				require.NoError(t, err)
				r.Header.Set("Authorization", testAuthorization)
				r.Header.Set("Content-Type", "application/json")
				response, err := http.DefaultClient.Do(r)
				require.NoError(t, err)
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				require.EqualValues(t, "BYPASS", response.Header.Get("X-GitCDN-Cache-Status"), "LFS alias route rejected: %d", response.StatusCode)
				require.NotEqualValues(t, 400, response.StatusCode, "LFS alias route rejected: %d", response.StatusCode)
			}
		})
	}
}

func TestYAMLConfiguredSingleSiteAuthentication(t *testing.T) {
	f := newFixture(t)
	s, h := newYAMLCDN(t, fmt.Sprintf("mode: single\nupstream:\n  url: %q\n", f.origin.URL+"/root"))
	for _, suffix := range []string{"", ".git"} {
		r := postFetch(t, h.URL, "/Group/Repo"+suffix+"/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
		responseBody(t, r)
	}
	require.EqualValues(t, 2, f.probes.Load(), "single-site aliases skipped authorization or did not share cache")
	require.EqualValues(t, 1, s.hydrated.Load(), "single-site aliases skipped authorization or did not share cache")
	f.denied.Store(true)
	r := postFetch(t, h.URL, "/Group/Repo/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.EqualValues(t, 401, r.StatusCode, "single-site mode served revoked credentials")
	require.EqualValues(t, 2, s.local.Load(), "single-site mode served revoked credentials")
}

func TestYAMLConfiguredMultiSiteIsolation(t *testing.T) {
	first, second := newFixture(t), newFixture(t)
	testGit(t, second.repo, "fetch", "--quiet", "--no-tags", first.repo, "+"+first.tip+":refs/heads/main")
	s, h := newYAMLCDN(t, fmt.Sprintf("mode: multi\nupstreams:\n  one:\n    url: %q\n  two:\n    url: %q\n", first.origin.URL+"/root", second.origin.URL+"/root"))
	for _, site := range []string{"one", "two"} {
		r := postFetch(t, h.URL, "/"+site+"/Group/Repo/git-upload-pack", fetchBody(first.tip), testAuthorization, nil)
		require.EqualValues(t, "MISS", r.Header.Get("X-GitCDN-Cache-Status"), "different sites shared cached objects")
		responseBody(t, r)
	}
	require.EqualValues(t, 2, s.hydrated.Load(), "multi-site requests used the wrong origin")
	require.EqualValues(t, 1, first.probes.Load(), "multi-site requests used the wrong origin")
	require.EqualValues(t, 1, second.probes.Load(), "multi-site requests used the wrong origin")
	first.denied.Store(true)
	r := postFetch(t, h.URL, "/one/Group/Repo/git-upload-pack", fetchBody(first.tip), testAuthorization, nil)
	require.EqualValues(t, 401, r.StatusCode, "first site did not enforce its own authorization")
	r = postFetch(t, h.URL, "/two/Group/Repo.git/git-upload-pack", fetchBody(first.tip), testAuthorization, nil)
	require.EqualValues(t, "HIT", r.Header.Get("X-GitCDN-Cache-Status"), "first site's authorization affected second site's cache")
	responseBody(t, r)
	second.commit(t, "second site")
	testGit(t, second.work, "push", "--quiet", "--force", second.repo, "main")
	clone := filepath.Join(t.TempDir(), "two")
	testGit(t, "", "clone", "--quiet", h.URL+"/two/Group/Repo", clone)
	require.EqualValues(t, second.tip, testGit(t, clone, "rev-parse", "HEAD"), "clone returned data from a different site")
	r = postFetch(t, h.URL, "/unknown/Group/Repo/git-upload-pack", fetchBody(second.tip), testAuthorization, nil)
	require.EqualValues(t, 400, r.StatusCode, "unknown site was not rejected")
}

func TestSingleSiteRedirectAndGroupNames(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "/base/Group/Renamed.git/info/refs?service=git-upload-pack")
		w.WriteHeader(http.StatusMovedPermanently)
	}))
	defer origin.Close()
	s, h := newYAMLCDN(t, fmt.Sprintf("mode: single\nupstream:\n  url: %q\n", origin.URL+"/base"))
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r, err := client.Get(h.URL + "/Group/Repo/info/refs?service=git-upload-pack")
	require.NoError(t, err)
	defer r.Body.Close()
	require.EqualValues(t, "/Group/Renamed.git/info/refs?service=git-upload-pack", r.Header.Get("Location"), "single-site redirect added a site prefix: %s", r.Header.Get("Location"))
	for _, group := range []string{"gitlab", "default"} {
		rt, err := s.resolve(httptest.NewRequest("GET", "/"+group+"/Repo/info/refs", nil))
		require.NoError(t, err, "single-site group interpreted as site ID: %+v %v", rt, err)
		require.EqualValues(t, group+"/Repo.git", rt.repo, "single-site group interpreted as site ID: %+v %v", rt, err)
		require.EqualValues(t, "", rt.prefix, "single-site group interpreted as site ID: %+v %v", rt, err)
	}
}
