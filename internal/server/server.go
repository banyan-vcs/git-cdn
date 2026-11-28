package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime/debug"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
)

type Server struct {
	config    config.Config
	git       *nativegit.Runner
	store     *cache.Store
	transport *http.Transport
	// probeTransport serves authorization probes and GitHub metadata only.
	// Passthrough streams — pushes, v0/v1 fetches — hold their connection
	// for a whole transfer; sharing one pool let a burst of them queue
	// every probe behind it until upstream_timeout.
	probeTransport *http.Transport
	client         *http.Client
	upstreams      map[string]*url.URL
	githubAPI      *url.URL
	logger         *slog.Logger
	started        time.Time
	// tune holds the reloadable scalars; a reload swaps the whole snapshot
	// and every reader loads it per request, so no field races a write.
	tune      atomic.Pointer[Tuning]
	requests  atomic.Pointer[requestLimiter]
	responses *responseBudget
	// drain rejects new work on both frontends; in-flight transfers keep
	// their grace. sshSessions counts live SSH connections when the SSH
	// frontend registers its counter.
	drain          atomic.Bool
	sshSessions    func() int
	configPath     string
	trustedProxies []*net.IPNet
	proxyAllow     []string
	probes         probeCache
	fanouts        fanoutRegistry
	flights        flightCache
	generations    generationFailures
	rejected       atomic.Uint64
	local          atomic.Uint64
	hydrated       atomic.Uint64
	proxied        atomic.Uint64
	authDenied     atomic.Uint64
	probeHits      atomic.Uint64
	probeMerged    atomic.Uint64
	packHits       atomic.Uint64
	packStores     atomic.Uint64
	fanoutSubs     atomic.Uint64
	fanoutDetached atomic.Uint64
	flightWaits    atomic.Uint64
}

// Tuning is the scalar set a reload may change on the running node. Both
// frontends read it per request; everything outside it needs a restart.
type Tuning struct {
	MaxRequestBytes  int64
	MaxRefsBytes     int64
	GitTimeout       time.Duration
	FetchTimeout     time.Duration
	AuthTimeout      time.Duration
	WriteIdleTimeout time.Duration
	RequestTimeout   time.Duration
}

func newTuning(c config.Config) *Tuning {
	t := &Tuning{MaxRequestBytes: c.MaxRequestBytes, MaxRefsBytes: c.MaxRefsBytes}
	t.GitTimeout, _ = time.ParseDuration(c.GitTimeout)
	t.FetchTimeout, _ = time.ParseDuration(c.FetchTimeout)
	t.AuthTimeout, _ = time.ParseDuration(c.UpstreamTimeout)
	t.WriteIdleTimeout, _ = time.ParseDuration(c.WriteIdleTimeout)
	t.RequestTimeout, _ = time.ParseDuration(c.RequestTimeout)
	return t
}

// CurrentTuning snapshots the reloadable set for the SSH frontend, which
// shares it with the HTTP side.
func (s *Server) CurrentTuning() Tuning { return *s.tune.Load() }

func New(c config.Config, logger *slog.Logger) (*Server, error) {
	if logger == nil {
		logger = slog.Default()
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	git, err := nativegit.New(c.GitBinary, c.MaxConcurrentGit, nativegit.WithLogger(logger))
	if err != nil {
		return nil, err
	}
	checkCtx, checkCancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = git.RequireReftable(checkCtx)
	checkCancel()
	if err != nil {
		return nil, err
	}
	repackTimeout, _ := time.ParseDuration(c.RepackTimeout)
	gcTimeout, _ := time.ParseDuration(c.GCTimeout)
	gcInterval, _ := time.ParseDuration(c.GCInterval)
	gcRetention, _ := time.ParseDuration(c.GCRetention)
	store, err := cache.Open(c.CacheDir, git, c.MaxCacheBytes, cache.RepackOptions{PackThreshold: c.RepackPackThreshold, Timeout: repackTimeout, GCTimeout: gcTimeout, GCInterval: gcInterval, GCRetention: gcRetention})
	if err != nil {
		return nil, err
	}
	store.SetPackCacheBytes(c.MaxPackCacheBytes)
	transport, err := outboundTransport(c)
	if err != nil {
		store.Close()
		return nil, err
	}
	probeTransport := transport.Clone()
	s := &Server{
		config:         c,
		git:            git,
		store:          store,
		transport:      transport,
		probeTransport: probeTransport,
		logger:         logger,
		started:        time.Now(),
		upstreams:      make(map[string]*url.URL),
		responses:      &responseBudget{limit: c.MaxTempResponseBytes},
	}
	s.requests.Store(newRequestLimiter(c.MaxConcurrentRequests, c.MaxRequestsPerSecond))
	s.tune.Store(newTuning(c))
	s.probes.init(parseProbeTTL(c.AuthProbeCacheTTL))
	// Every concurrent stream may hold one block and queue two more; the
	// pool follows the configured concurrency so raising the limits does
	// not leave a fast broadcast waiting on slow readers' slots.
	streams := c.MaxConcurrentRequests
	if c.SSH != nil {
		streams += c.SSH.MaxConnections * sshSessionsPerConnection
	}
	s.fanouts.init(streams)
	// A soft memory limit makes the collector work harder as the heap
	// approaches it instead of growing into an out-of-memory kill. It
	// bounds this process only; child Git processes have their own limits.
	if c.MemoryLimitBytes > 0 {
		debug.SetMemoryLimit(c.MemoryLimitBytes)
	}
	for _, cidr := range c.TrustedProxyCIDRs {
		_, network, _ := net.ParseCIDR(cidr)
		s.trustedProxies = append(s.trustedProxies, network)
	}
	s.client = &http.Client{Transport: s.probeTransport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	s.githubAPI, _ = url.Parse("https://api.github.com")
	upstreams := c.Upstreams
	if c.Mode == config.ModeSingle {
		upstreams = map[string]config.Upstream{config.SingleUpstreamID: *c.Upstream}
	}
	for id, upstream := range upstreams {
		s.upstreams[id], _ = url.Parse(strings.TrimSuffix(upstream.URL, "/"))
	}
	for i := range s.config.AuthHeaders {
		s.config.AuthHeaders[i] = http.CanonicalHeaderKey(s.config.AuthHeaders[i])
	}
	sort.Strings(s.config.AuthHeaders)
	s.proxyAllow = append(append([]string{}, proxyHeaders...), s.config.AuthHeaders...)
	s.probes.startJanitor()
	return s, nil
}

// sshSessionsPerConnection mirrors the SSH frontend's per-connection
// session bound when its configuration is present.
const sshSessionsPerConnection = 4

// outboundTransport builds the origin transport. Environment proxy
// variables stay ignored: egress follows configuration only, so a proxy or
// a private CA is an explicit deployment decision and never a surprise
// from the service user's shell.
func outboundTransport(c config.Config) (*http.Transport, error) {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.ResponseHeaderTimeout = 30 * time.Second
	t.MaxConnsPerHost = c.MaxConcurrentRequests
	if c.UpstreamProxy != "" {
		proxy, err := url.Parse(c.UpstreamProxy)
		if err != nil {
			return nil, fmt.Errorf("upstream_proxy: %w", err)
		}
		// http.ProxyURL would send every request to the proxy. A loopback
		// origin shares the host with this process and is never proxied,
		// as with proxy variables from the environment.
		t.Proxy = func(request *http.Request) (*url.URL, error) {
			if host := request.URL.Hostname(); host == "localhost" || net.ParseIP(host).IsLoopback() {
				return nil, nil
			}
			return proxy, nil
		}
	}
	if c.UpstreamCAFile != "" {
		pem, err := os.ReadFile(c.UpstreamCAFile)
		if err != nil {
			return nil, fmt.Errorf("upstream_ca_file: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("upstream_ca_file contains no PEM certificates")
		}
		// Keep the clone's protocol negotiation; only the roots change.
		t.TLSClientConfig = t.TLSClientConfig.Clone()
		t.TLSClientConfig.RootCAs = roots
	}
	return t, nil
}

// Close must be called after the HTTP server has stopped handling requests.
func (s *Server) Close() error {
	s.transport.CloseIdleConnections()
	s.probeTransport.CloseIdleConnections()
	s.probes.stopJanitor()
	// Pumps stop and detach their subscribers, so in-flight transfers fall
	// back to their own file reads instead of truncating.
	s.fanouts.close()
	return s.store.Close()
}
