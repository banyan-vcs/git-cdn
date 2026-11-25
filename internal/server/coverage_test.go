package server

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
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

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"

	"github.com/stretchr/testify/require"
)

// capturingCDN serves with a JSON logger at DEBUG and returns the buffer
// its events accumulate in.
func capturingCDN(t *testing.T, f *fixture, modify func(*config.Config)) (*Server, *httptest.Server, *eventBuffer) {
	t.Helper()
	output := &eventBuffer{}
	s, h := newCDNWithLogger(t, f, modify, slog.New(slog.NewJSONHandler(output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return s, h, output
}

func loggedEvents(output *eventBuffer) []map[string]any {
	output.Lock()
	defer output.Unlock()
	var events []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for {
		var event map[string]any
		if err := decoder.Decode(&event); err != nil {
			break
		}
		events = append(events, event)
	}
	return events
}

func countEvents(events []map[string]any, message string) int {
	count := 0
	for _, event := range events {
		if event["msg"] == message {
			count++
		}
	}
	return count
}

// A default `git clone` wants every advertised ref. A repository with more
// branches than the old 64-want cap used to pass through for its whole
// clone; it now hydrates once and serves every later clone from the cache.
func TestDefaultCloneOfManyRefRepositoryUsesCache(t *testing.T) {
	f := newFixture(t)
	var updates bytes.Buffer
	for i := range 100 {
		fmt.Fprintf(&updates, "create refs/heads/topic-%03d %s\n", i, f.tip)
	}
	cmd := exec.Command("git", "-C", f.repo, "update-ref", "--stdin")
	cmd.Stdin = &updates
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "bulk branches: %s", out)
	s, h := newCDN(t, f, nil)
	cloneURL := h.URL + "/gitlab/Group/Repo.git"
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	testGit(t, "", "clone", "--quiet", cloneURL, first)
	require.EqualValues(t, 1, s.local.Load(), "the clone of a many-ref repository bypassed the cache")
	require.Len(t, strings.Split(testGit(t, first, "branch", "-r"), "\n"), 102, "remote branches were not cloned")
	fetches := f.fetches.Load()
	testGit(t, "", "clone", "--quiet", cloneURL, second)
	require.EqualValues(t, 2, s.local.Load(), "the second clone bypassed the cache")
	require.Equal(t, fetches, f.fetches.Load(), "the second clone fetched objects from upstream")
	require.Equal(t, "v1", testGit(t, second, "tag"), "annotated tag lost")
}

// Git sends a developer's unpushed commits as haves before anything the
// cache knows. They are dropped from the request instead of sending the
// whole fetch upstream; the fetch is still correct.
func TestUnpushedCommitsInHavesStayLocal(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	client := filepath.Join(t.TempDir(), "client")
	testGit(t, "", "clone", "--quiet", h.URL+"/gitlab/Group/Repo.git", client)
	f.commit(t, "updated")
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	testGit(t, client, "checkout", "--quiet", "-b", "wip")
	testGit(t, client, "commit", "--quiet", "--allow-empty", "-m", "local work")
	testGit(t, client, "checkout", "--quiet", "main")
	local, proxied := s.local.Load(), s.proxied.Load()
	testGit(t, client, "fetch", "--quiet", "origin")
	require.Equal(t, f.tip, testGit(t, client, "rev-parse", "origin/main"), "fetch did not reach the new tip")
	require.Equal(t, local+1, s.local.Load(), "unpushed local commits pushed the fetch upstream")
	// Discovery and ls-refs pass through; the fetch itself did not.
	require.Equal(t, proxied+2, s.proxied.Load(), "the fetch request was forwarded")
	// The unknown have was never handed to upload-pack: it cannot be
	// acknowledged, and the client's own commit is untouched.
	require.Equal(t, "local work", testGit(t, client, "log", "-1", "--format=%s", "wip"))
}

// A hydration that fails is not repeated for every request: the scope backs
// off, and requests inside the backoff pass through without waiting.
func TestHydrationFailureBacksOff(t *testing.T) {
	f := newFixture(t)
	_, h, output := capturingCDN(t, f, func(c *config.Config) { c.MaxCacheBytes = 1; c.MaxPackCacheBytes = -1 })
	body := fetchBody(f.tip)
	for range 3 {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
		require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"))
		require.Contains(t, string(responseBody(t, r)), "packfile\n")
	}
	events := loggedEvents(output)
	require.Equal(t, 1, countEvents(events, "cache hydration failed"), "a failed hydration was attempted again")
	require.Equal(t, 2, countEvents(events, "local fetch preparation deferred"), "requests inside the backoff were not deferred")
}

// A response larger than max_response_bytes cannot be produced by hydrating
// either: it is attempted once per backoff period, not twice per request.
func TestOversizedResponseIsNotRegeneratedPerRequest(t *testing.T) {
	f := newFixture(t)
	s, h, output := capturingCDN(t, f, func(c *config.Config) { c.MaxResponseBytes = 16 })
	body := fetchBody(f.tip)
	for range 3 {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
		require.EqualValues(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"))
		require.Equal(t, 1, bytes.Count(responseBody(t, r), []byte("packfile\n")), "fallback appended a second response")
	}
	events := loggedEvents(output)
	require.Equal(t, 1, countEvents(events, "local pack generation failed"), "an impossible response was generated again")
	require.Equal(t, 2, countEvents(events, "local fetch preparation deferred"))
	require.Zero(t, s.local.Load())
}

// The agent line names the client's Git version and never changes the
// response: clients on different versions share one stored entry.
func TestPackCacheKeyIgnoresAgent(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	for i, agent := range []string{"git/2.40.0", "git/2.50.0.windows.1-Windows", ""} {
		var b bytes.Buffer
		// The agent line is a capability and precedes the argument section.
		b.Write(protocol.Encode("command=fetch\n"))
		if agent != "" {
			b.Write(protocol.Encode("agent=" + agent + "\n"))
		}
		b.WriteString("0001")
		b.Write(protocol.Encode("want " + f.tip + "\n"))
		b.Write(protocol.Encode("done\n"))
		b.WriteString("0000")
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", b.Bytes(), testAuthorization, nil)
		responseBody(t, r)
		expected := "HIT"
		if i == 0 {
			expected = "MISS"
		}
		require.Equal(t, expected, r.Header.Get("X-GitCDN-Pack-Cache"), "agent %q: %v", agent, r.Header)
	}
	require.EqualValues(t, 1, s.packStores.Load(), "one response was stored per client version")
	require.EqualValues(t, 2, s.packHits.Load())
}

// Git does not retry a 503. A burst over the rate limit waits for a token
// instead of failing; a burst beyond the waiting room is still rejected.
func TestAdmissionQueuesShortBurst(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxRequestsPerSecond = 4 })
	path := "/gitlab/Group/Repo.git/git-upload-pack"
	body := fetchBody(f.tip)
	responseBody(t, postFetch(t, h.URL, path, body, testAuthorization, nil))
	burst := func(requests int) map[int]int {
		t.Helper()
		// Let the token bucket refill to its full burst.
		time.Sleep(1200 * time.Millisecond)
		statuses := make(chan int, requests)
		var wg sync.WaitGroup
		for range requests {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := postFetch(t, h.URL, path, body, testAuthorization, nil)
				io.Copy(io.Discard, r.Body)
				r.Body.Close()
				statuses <- r.StatusCode
			}()
		}
		wg.Wait()
		close(statuses)
		counts := make(map[int]int)
		for status := range statuses {
			counts[status]++
		}
		return counts
	}
	require.Equal(t, map[int]int{200: 6}, burst(6), "a burst the rate can absorb within the wait was rejected")
	flood := burst(12)
	require.Positive(t, flood[200], "no request was admitted under a flood")
	require.Positive(t, flood[503], "requests piled up beyond the waiting room")
	require.Zero(t, s.requests.active(), "admission slots leaked")
}

// A passthrough whose upstream response fails after its headers were
// committed is an interrupted transfer, not a panic of the service.
func TestInterruptedPassthroughIsNotAPanic(t *testing.T) {
	f := newFixture(t)
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-git-upload-pack-advertisement")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		io.WriteString(w, "001e# service=git-upload-pack\n")
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err == nil {
			conn.Close()
		}
	}))
	defer cut.Close()
	u, err := url.Parse(cut.URL)
	require.NoError(t, err)
	var output eventBuffer
	s, h := newCDNWithLogger(t, f, nil, slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	s.upstreams["gitlab"] = u
	response, err := http.Get(h.URL + "/gitlab/Group/Repo.git/info/refs?service=git-upload-pack")
	if err == nil {
		io.Copy(io.Discard, response.Body)
		response.Body.Close()
	}
	var summary, interrupted map[string]any
	for _, event := range loggedEvents(&output) {
		switch event["msg"] {
		case "repository request completed":
			summary = event
		case "upstream passthrough interrupted":
			interrupted = event
		}
	}
	require.NotNil(t, summary, "the request summary was not logged")
	require.Equal(t, false, summary["panicked"], "an interrupted upstream response was logged as a panic")
	require.NotNil(t, interrupted, "the interrupted transfer was not reported")
	require.Equal(t, "upstream_read", interrupted["side"])
	require.Equal(t, "unexpected_eof", interrupted["error_kind"])
}

// Lifetime entries leave the probe cache when their lifetime ends, without
// waiting to be looked up again or for the count limit to press.
func TestProbeLifetimeEntriesExpireOnTheirOwn(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "1s"; c.MaxPackCacheBytes = -1 })
	body := fetchBody(f.tip)
	for range 2 {
		responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	}
	s.probes.mu.Lock()
	require.Len(t, s.probes.entries, 1, "the successful probe was not remembered")
	for key, entry := range s.probes.entries {
		entry.expires = time.Now().Add(-time.Second)
		s.probes.entries[key] = entry
	}
	s.probes.mu.Unlock()
	require.Eventually(t, func() bool {
		s.probes.mu.Lock()
		defer s.probes.mu.Unlock()
		return len(s.probes.entries) == 0 && s.probes.refs == 0
	}, 4*time.Second, 20*time.Millisecond, "an expired snapshot stayed in memory")
}

// A request that holds a slot while its body stalls is disconnected by the
// idle read deadline and gives the slot back.
func TestStalledRequestBodyReleasesItsSlot(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) { c.WriteIdleTimeout = "200ms" })
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(h.URL, "http://"), 5*time.Second)
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprint(conn, "POST /gitlab/Group/Repo.git/git-upload-pack HTTP/1.1\r\nHost: localhost\r\nContent-Type: application/x-git-upload-pack-request\r\nGit-Protocol: version=2\r\nContent-Length: 100\r\n\r\nabcd")
	require.NoError(t, err)
	require.Eventually(t, func() bool { return s.requests.active() == 1 }, 5*time.Second, 10*time.Millisecond, "the request did not take a slot")
	require.Eventually(t, func() bool { return s.requests.active() == 0 }, 5*time.Second, 20*time.Millisecond, "a stalled body kept its slot")
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, err = conn.Read(make([]byte, 1))
	require.Error(t, err, "the stalled connection stayed open")
}

// Egress follows configuration only. A configured proxy and private CA
// reach both origin connection pools, proxy variables from the environment
// reach neither, and the pools themselves are separate so passthrough
// streams never queue an authorization probe.
func TestOutboundTransportFollowsConfiguration(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://environment-proxy.example:3128")
	t.Setenv("https_proxy", "http://environment-proxy.example:3128")
	direct, err := outboundTransport(config.Config{})
	require.NoError(t, err)
	require.Nil(t, direct.Proxy, "a proxy variable from the environment was honored")
	require.Nil(t, direct.TLSClientConfig.RootCAs, "a default transport replaced the system roots")

	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	template := x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Origin CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, &template, &template, key.Public(), key)
	require.NoError(t, err)
	bundle := filepath.Join(t.TempDir(), "origin-ca.pem")
	require.NoError(t, os.WriteFile(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600))
	empty := filepath.Join(t.TempDir(), "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0600))
	_, err = outboundTransport(config.Config{UpstreamCAFile: empty})
	require.Error(t, err, "a file without certificates was accepted")

	configured, err := outboundTransport(config.Config{UpstreamProxy: "socks5://proxy.example:1080", UpstreamCAFile: bundle})
	require.NoError(t, err)
	require.NotNil(t, configured.Proxy)
	require.NotNil(t, configured.TLSClientConfig)
	require.NotNil(t, configured.TLSClientConfig.RootCAs)
	require.True(t, configured.ForceAttemptHTTP2, "replacing the roots changed protocol negotiation")
	// A loopback origin shares the host with this process and is never
	// sent through the proxy.
	proxied, err := configured.Proxy(&http.Request{URL: &url.URL{Scheme: "http", Host: "127.0.0.1:8080"}})
	require.NoError(t, err)
	require.Nil(t, proxied, "a loopback origin was sent through the proxy")
	proxied, err = configured.Proxy(&http.Request{URL: &url.URL{Scheme: "https", Host: "git.example.com"}})
	require.NoError(t, err)
	require.Equal(t, "proxy.example:1080", proxied.Host)

	f := newFixture(t)
	s, h := newCDN(t, f, func(c *config.Config) {
		c.UpstreamProxy = "socks5://proxy.example:1080"
		c.UpstreamCAFile = bundle
	})
	probes, passthrough := s.client.Transport.(*http.Transport), s.transport
	require.Same(t, s.probeTransport, probes, "probes do not use their own pool")
	require.NotSame(t, passthrough, probes, "probes share the passthrough pool")
	require.NotNil(t, probes.Proxy)
	require.NotNil(t, passthrough.Proxy)
	// A loopback origin is never proxied; the settings change nothing for it.
	require.Contains(t, string(responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))), "packfile\n")
}

// The broadcast block pool follows the configured stream limit: every
// subscriber may hold one block and queue two more.
func TestBroadcastPoolFollowsConcurrency(t *testing.T) {
	var small, large fanoutRegistry
	small.init(16)
	large.init(16 + 8*sshSessionsPerConnection)
	require.Equal(t, fanoutPoolBlocks, cap(small.slots))
	require.Equal(t, (16+8*sshSessionsPerConnection)*(1+fanoutQueueDepth), cap(large.slots))
}
