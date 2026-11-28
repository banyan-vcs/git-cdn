package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/testlog"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func githubTestServer(t *testing.T, metadata http.HandlerFunc) (*Server, *httptest.Server) {
	t.Helper()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="GitHub"`)
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, "authentication required\n")
	}))
	api := httptest.NewServer(metadata)
	s, err := New(config.Config{CacheDir: t.TempDir(), Upstreams: map[string]config.Upstream{"github": {URL: "https://github.com"}}}, testlog.New(t))
	require.NoError(t, err)
	s.githubAPI, _ = url.Parse(api.URL)
	// Passthrough and probes use separate connection pools; both must
	// reach the local GitHub stub.
	tlsConfig := origin.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	// Only this local GitHub stub uses a certificate issued for another hostname.
	tlsConfig.InsecureSkipVerify = true
	for _, transport := range []*http.Transport{s.transport, s.probeTransport} {
		dial := transport.DialContext
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			if address == "github.com:443" {
				address = origin.Listener.Addr().String()
			}
			return dial(ctx, network, address)
		}
		transport.TLSClientConfig = tlsConfig.Clone()
	}
	h := httptest.NewServer(s)
	t.Cleanup(func() { h.Close(); s.Close(); api.Close(); origin.Close() })
	return s, h
}

func TestGitHubAbsentOwnerReturnsNotFoundWithoutChallenge(t *testing.T) {
	s, h := githubTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.EqualValues(t, "/users/banyan-xxx", r.URL.Path, "unexpected metadata path")
		assert.False(t, r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "", "caller credentials forwarded to public metadata lookup")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(404)
		io.WriteString(w, `{"message":"Not Found"}`)
	})
	for _, suffix := range []string{"", ".git"} {
		r, err := http.Get(h.URL + "/github/banyan-xxx/banyan" + suffix + "/info/refs?service=git-upload-pack")
		require.NoError(t, err)
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		require.EqualValues(t, 404, r.StatusCode, "missing repository returned login challenge: %d %s", r.StatusCode, body)
		require.EqualValues(t, "", r.Header.Get("WWW-Authenticate"), "missing repository returned login challenge: %d %s", r.StatusCode, body)
		require.EqualValues(t, "repository not found\n", string(body), "missing repository returned login challenge: %d %s", r.StatusCode, body)
	}
	r := postFetch(t, h.URL, "/github/banyan-xxx/banyan/git-upload-pack", fetchBody(strings.Repeat("a", 40)), "Bearer caller-secret", nil)
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	require.EqualValues(t, 404, r.StatusCode, "direct fetch did not normalize missing repository: %d %s", r.StatusCode, body)
	require.EqualValues(t, "", r.Header.Get("WWW-Authenticate"), "direct fetch did not normalize missing repository: %d %s", r.StatusCode, body)
	require.EqualValues(t, "repository not found\n", string(body), "direct fetch did not normalize missing repository: %d %s", r.StatusCode, body)
	require.EqualValues(t, 0, s.local.Load(), "missing repository entered cache")
	require.EqualValues(t, 0, s.store.Stats().Bytes, "missing repository entered cache")
}

func TestGitHubUnknownVisibilityPreservesUnauthorized(t *testing.T) {
	for _, test := range []struct {
		name              string
		status            int
		contentType, body string
	}{
		{"existing owner", 200, "application/json", `{"login":"known-owner"}`},
		{"rate limit", 403, "application/json", `{"message":"rate limit"}`},
		{"upstream failure", 500, "application/json", `{"message":"failure"}`},
		{"invalid HTML", 404, "text/html", `<html>not found</html>`},
		{"invalid JSON", 404, "application/json", `{"message":"other"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, h := githubTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				io.WriteString(w, test.body)
			})
			r, err := http.Get(h.URL + "/github/known-owner/private.git/info/refs?service=git-upload-pack")
			require.NoError(t, err)
			body, _ := io.ReadAll(r.Body)
			r.Body.Close()
			require.EqualValues(t, 401, r.StatusCode, "ambiguous/private repository authorization was rewritten")
			require.NotEqualValues(t, "", r.Header.Get("WWW-Authenticate"), "ambiguous/private repository authorization was rewritten")
			require.EqualValues(t, "authentication required\n", string(body), "ambiguous/private repository authorization was rewritten")
		})
	}
}

func TestGitHubNormalizerDoesNotChangeOtherHosts(t *testing.T) {
	s, h := githubTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Fail(t, "test failed", "other host triggered GitHub metadata lookup")
	})
	u, _ := url.Parse("https://gitlab.com")
	r, err := http.NewRequest("GET", h.URL, nil)
	require.NoError(t, err)
	response := &http.Response{StatusCode: 401, Header: http.Header{"WWW-Authenticate": {`Basic realm="Origin"`}}, Body: io.NopCloser(strings.NewReader("authentication required"))}
	require.False(t, s.normalizeGitHubNotFound(r, route{base: u, repo: "owner/repo.git"}, response), "other upstream status changed")
	require.EqualValues(t, 401, response.StatusCode, "other upstream status changed")
}
