package server

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/testlog"

	"github.com/stretchr/testify/require"
)

func TestUnauthenticatedPacketFloodHasBoundedAllocations(t *testing.T) {
	var calls atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer origin.Close()
	s, _ := newCDN(t, &fixture{origin: origin}, nil)
	data := bytes.Repeat([]byte("0000"), (8<<20)/4)
	r := httptest.NewRequest("POST", "/gitlab/Group/Repo.git/git-upload-pack", bytes.NewReader(data))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Git-Protocol", "version=2")
	w := httptest.NewRecorder()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s.ServeHTTP(w, r)
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	t.Logf("8 MiB unauthenticated flood: status=%d, allocated=%d bytes", w.Code, allocated)
	require.EqualValues(t, 400, w.Code, "packet flood was not bounded: status=%d calls=%d allocated=%d", w.Code, calls.Load(), allocated)
	require.EqualValues(t, 0, calls.Load(), "packet flood was not bounded: status=%d calls=%d allocated=%d", w.Code, calls.Load(), allocated)
	require.False(t, allocated > uint64(len(data))*20, "packet flood was not bounded: status=%d calls=%d allocated=%d", w.Code, calls.Load(), allocated)
}

type stalledWriter struct {
	header  http.Header
	ready   chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (w *stalledWriter) Header() http.Header { return w.header }
func (w *stalledWriter) WriteHeader(int)     {}
func (w *stalledWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.ready) })
	<-w.release
	return len(p), nil
}

func TestResponseBudgetStaysChargedUntilTransferEnds(t *testing.T) {
	f := newFixture(t)
	// This test exercises the spool budget; response-pack reuse streams
	// persistent files and deliberately does not consume it.
	s, h := newCDN(t, f, func(c *config.Config) { c.MaxConcurrentGit = 1; c.MaxPackCacheBytes = -1 })
	body := fetchBody(f.tip, "no-progress")
	warm := responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	// Each response is generated again, and two generations of the same
	// fetch differ by a few bytes of compression. The slack covers that and
	// is still far less than one more response.
	s.responses.limit = int64(len(warm)*2 + 64)
	release := make(chan struct{})
	var releaseOnce sync.Once
	var wg sync.WaitGroup
	defer func() { releaseOnce.Do(func() { close(release) }); wg.Wait() }()
	for i := range 3 {
		w := &stalledWriter{header: make(http.Header), ready: make(chan struct{}), release: release}
		r := httptest.NewRequest("POST", "/gitlab/Group/Repo.git/git-upload-pack", bytes.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", testAuthorization)
		r.Header.Set("Git-Protocol", "version=2")
		r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		wg.Add(1)
		go func() { defer wg.Done(); s.ServeHTTP(w, r) }()
		select {
		case <-w.ready:
		case <-time.After(15 * time.Second):
			require.FailNow(t, "test failed", "response did not start")
		}
		expected := "HIT"
		if i == 2 {
			expected = "BYPASS"
		}
		require.EqualValues(t, expected, w.header.Get("X-GitCDN-Cache-Status"), "response %d: %v", i, w.header)
	}
	files, err := filepath.Glob(filepath.Join(s.store.Tmp, "response-*"))
	require.NoError(t, err, "retained responses escaped the budget: files=%d bytes=%d requests=%d err=%v", len(files), s.responses.bytes(), s.requests.Load().active(), err)
	require.Len(t, files, 2, "retained responses escaped the budget: files=%d bytes=%d requests=%d err=%v", len(files), s.responses.bytes(), s.requests.Load().active(), err)
	// Every spooled byte stays charged until its transfer ends, the charge
	// never passes the budget, and what is left cannot hold a third response.
	var spooled int64
	for _, file := range files {
		info, err := os.Stat(file)
		require.NoError(t, err)
		spooled += info.Size()
	}
	charge, limit := s.responses.bytes(), s.responses.limit
	require.EqualValues(t, spooled, charge, "spooled bytes and the charge differ: files=%d spooled=%d charge=%d limit=%d", len(files), spooled, charge, limit)
	require.LessOrEqual(t, charge, limit, "retained responses escaped the budget: spooled=%d charge=%d limit=%d", spooled, charge, limit)
	require.Less(t, limit-charge, int64(len(warm)), "a third response still fits: spooled=%d charge=%d limit=%d", spooled, charge, limit)
	require.EqualValues(t, 3, s.requests.Load().active(), "retained responses escaped the budget: files=%d bytes=%d requests=%d err=%v", len(files), s.responses.bytes(), s.requests.Load().active(), err)
	releaseOnce.Do(func() { close(release) })
	wg.Wait()
	files, err = filepath.Glob(filepath.Join(s.store.Tmp, "response-*"))
	require.NoError(t, err, "completed responses retained disk or request reservations")
	require.Len(t, files, 0, "completed responses retained disk or request reservations")
	require.EqualValues(t, 0, s.responses.bytes(), "completed responses retained disk or request reservations")
	require.EqualValues(t, 0, s.requests.Load().active(), "completed responses retained disk or request reservations")
}

type unreadBody struct{}

func (unreadBody) Read([]byte) (int, error) { panic("overload read a request body") }

func TestRequestOverloadRejectsBeforeBodyAndKeepsHealthAvailable(t *testing.T) {
	f := newFixture(t)
	s, _ := newCDN(t, f, func(c *config.Config) { c.MaxConcurrentRequests = 1 })
	// The queue wait is exercised separately; here the slot stays taken.
	s.requests.Load().wait = 0
	require.True(t, s.requests.Load().acquire(context.Background()), "cannot acquire request slot")
	defer s.requests.Load().release()
	r := httptest.NewRequest("POST", "/gitlab/Group/Repo.git/git-upload-pack", unreadBody{})
	r.RemoteAddr = "127.0.0.1:1234"
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	require.EqualValues(t, 503, w.Code, "overload was not rejected before processing")
	require.EqualValues(t, "1", w.Header().Get("Retry-After"), "overload was not rejected before processing")
	require.EqualValues(t, 0, f.probes.Load(), "overload was not rejected before processing")
	for _, path := range []string{"/healthz", "/metrics"} {
		r := httptest.NewRequest("GET", path, nil)
		r.RemoteAddr = "127.0.0.1:1234"
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		require.EqualValues(t, 200, w.Code, "%s unavailable under overload", path)
	}
}

// Rate and concurrency limits are enforced without a wait; the handler
// adds the bounded queue on top (TestRequestAdmissionQueuesForCapacity).
func TestRequestRateLimitRefills(t *testing.T) {
	l := newRequestLimiter(1, 2)
	l.wait = 0
	ctx := context.Background()
	for range 2 {
		require.True(t, l.acquire(ctx), "initial burst rejected")
		l.release()
	}
	require.False(t, l.acquire(ctx), "unbounded request burst")
	l.mu.Lock()
	l.tokens += 1 // one 500ms refill step of a 2/s rate
	l.mu.Unlock()
	require.True(t, l.acquire(ctx), "rate budget did not refill")
	l.release()
}

// Git never retries a 503. A request over the rate limit waits for the next
// token instead of failing, but only within the admission wait and only if
// it fits the waiting room.
func TestRequestAdmissionQueuesForCapacity(t *testing.T) {
	l := newRequestLimiter(4, 2)
	l.wait = 2 * time.Second
	require.True(t, l.acquire(context.Background()), "initial burst rejected")
	require.True(t, l.acquire(context.Background()), "initial burst rejected")
	l.release()
	l.release()
	started := time.Now()
	require.True(t, l.acquire(context.Background()), "a token arriving inside the wait was not taken")
	require.GreaterOrEqual(t, time.Since(started), 400*time.Millisecond, "admitted before a token accrued")
	l.release()

	l = newRequestLimiter(1, 1)
	l.wait = 50 * time.Millisecond
	require.True(t, l.acquire(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() { done <- l.acquire(ctx) }()
	cancel()
	select {
	case admitted := <-done:
		require.False(t, admitted, "a canceled waiter was admitted")
	case <-time.After(2 * time.Second):
		require.FailNow(t, "test failed", "a canceled waiter kept queueing")
	}
	l.release()

	l = newRequestLimiter(1, 1)
	l.wait = 50 * time.Millisecond
	require.True(t, l.acquire(context.Background()))
	// The waiting room holds one burst: a second request queues, a third
	// is rejected at once.
	queued := make(chan bool, 2)
	go func() { queued <- l.acquire(context.Background()) }()
	require.Eventually(t, func() bool { return len(l.queue) == 1 }, time.Second, time.Millisecond)
	started = time.Now()
	require.False(t, l.acquire(context.Background()), "queue admitted beyond one burst")
	require.Less(t, time.Since(started), 20*time.Millisecond, "a request beyond the queue waited")
	require.False(t, <-queued, "a waiter without capacity was admitted after its wait")
	l.release()
}

func TestTemporaryResponseFailureReleasesBudget(t *testing.T) {
	b := &responseBudget{limit: 4}
	file, err := os.CreateTemp(t.TempDir(), "response-")
	require.NoError(t, err)
	f := &responseFile{file: file, budget: b, limit: 4}
	{
		_, err := f.Write([]byte("1234"))
		require.NoError(t, err)
	}
	{
		_, err := f.Write([]byte("5"))
		require.Error(t, err, "response exceeded its reservation")
		require.EqualValues(t, 4, b.bytes(), "response exceeded its reservation")
	}
	for range 2 {
		require.NoError(t, f.Close())
	}
	require.EqualValues(t, 0, b.bytes(), "response reservation leaked")
	{
		_, err := os.Stat(file.Name())
		require.True(t, os.IsNotExist(err), "response file retained after close")
	}
}

func TestHTTPSAndTrustedProxyEnforcement(t *testing.T) {
	f := newFixture(t)
	s, _ := newCDN(t, f, func(c *config.Config) { c.TrustedProxyCIDRs = []string{"10.0.0.0/8"} })
	for _, test := range []struct {
		name, peer, proto string
		tls               bool
		status            int
	}{
		{"loopback", "127.0.0.1:1234", "", false, 200},
		{"direct TLS", "198.51.100.1:1234", "", true, 200},
		{"trusted TLS proxy", "10.1.2.3:1234", "https", false, 200},
		{"spoofed proxy", "198.51.100.1:1234", "https", false, 403},
		{"trusted plaintext proxy", "10.1.2.3:1234", "http", false, 403},
		{"ambiguous protocol", "10.1.2.3:1234", "https,http", false, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/metrics", nil)
			r.RemoteAddr = test.peer
			r.Header.Set("X-Forwarded-Proto", test.proto)
			if test.tls {
				r.TLS = &tls.ConnectionState{}
			}
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			require.EqualValues(t, test.status, w.Code, "status=%d", w.Code)
		})
	}
	r := httptest.NewRequest("GET", "/metrics", nil)
	r.RemoteAddr = "10.1.2.3:1234"
	r.Header["X-Forwarded-Proto"] = []string{"https", "http"}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	require.EqualValues(t, 403, w.Code, "ambiguous forwarded protocol headers accepted")
}

func TestCredentialHostPreventsCrossUpstreamForwarding(t *testing.T) {
	var calls atomic.Int64
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); io.WriteString(w, "origin") }))
	defer origin.Close()
	c := config.Config{CacheDir: t.TempDir(), Upstreams: map[string]config.Upstream{
		"one": {URL: origin.URL, CredentialHost: "one.cdn.example"},
		"two": {URL: origin.URL, CredentialHost: "two.cdn.example"},
	}}
	s, err := New(c, testlog.New(t))
	require.NoError(t, err)
	defer s.Close()
	for _, test := range []struct {
		host   string
		status int
	}{{"one.cdn.example", 421}, {"two.cdn.example", 200}} {
		r := httptest.NewRequest("GET", "http://"+test.host+"/two/Group/Repo.git/info/refs", nil)
		r.RemoteAddr = "127.0.0.1:1234"
		r.Header.Set("Authorization", "Bearer user-token")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		require.EqualValues(t, test.status, w.Code, "host=%s status=%d", test.host, w.Code)
		require.False(t, test.status == 421 && calls.Load() != 0, "credentials reached the wrong upstream")
	}
	require.EqualValues(t, 1, calls.Load(), "correct credential host did not reach origin")
}

func TestRequestTimeoutCancelsStalledUpstream(t *testing.T) {
	canceled := make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		close(canceled)
	}))
	defer origin.Close()
	s, h := newCDN(t, &fixture{origin: origin}, func(c *config.Config) { c.RequestTimeout = "100ms" })
	r, err := http.Get(h.URL + "/gitlab/Group/Repo.git/info/refs")
	if err == nil {
		io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		require.FailNow(t, "test failed", "upstream survived request timeout")
	}
	require.EqualValues(t, 0, s.requests.Load().active(), "timed-out upstream retained a request slot")
}

func TestWriteIdleTimeoutStopsSlowClient(t *testing.T) {
	finished := make(chan struct{})
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		output := &idleResponseWriter{ResponseWriter: w, controller: http.NewResponseController(w), timeout: 50 * time.Millisecond}
		chunk := bytes.Repeat([]byte("x"), 64<<10)
		for range 2048 {
			if _, err := output.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer h.Close()
	conn, err := net.Dial("tcp", strings.TrimPrefix(h.URL, "http://"))
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))
	fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: test\r\n\r\n")
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		require.FailNow(t, "test failed", "slow client bypassed the write idle timeout")
	}
}

func TestWriteIdleTimeoutAllowsProgressAndConnectionReuse(t *testing.T) {
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		output := &idleResponseWriter{ResponseWriter: w, controller: http.NewResponseController(w), timeout: 100 * time.Millisecond}
		for range 5 {
			if _, err := output.Write(bytes.Repeat([]byte("x"), 32<<10)); err != nil {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
	}))
	defer h.Close()
	transport := http.DefaultTransport.(*http.Transport).Clone()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 3 * time.Second}
	for range 2 {
		r, err := client.Get(h.URL)
		require.NoError(t, err)
		n, err := io.Copy(io.Discard, r.Body)
		r.Body.Close()
		require.NoError(t, err, "progressing transfer stopped: bytes=%d err=%v", n, err)
		require.EqualValues(t, 5*(32<<10), n, "progressing transfer stopped: bytes=%d err=%v", n, err)
	}
}
