package server

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"net/url"
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
	config           config.Config
	git              *nativegit.Runner
	store            *cache.Store
	transport        *http.Transport
	client           *http.Client
	upstreams        map[string]*url.URL
	githubAPI        *url.URL
	logger           *slog.Logger
	gitTimeout       time.Duration
	fetchTimeout     time.Duration
	authTimeout      time.Duration
	writeIdleTimeout time.Duration
	requestTimeout   time.Duration
	requests         *requestLimiter
	responses        *responseBudget
	trustedProxies   []*net.IPNet
	proxyAllow       []string
	probes           probeCache
	fanouts          fanoutRegistry
	flights          flightCache
	rejected         atomic.Uint64
	local            atomic.Uint64
	hydrated         atomic.Uint64
	proxied          atomic.Uint64
	authDenied       atomic.Uint64
	probeHits        atomic.Uint64
	probeMerged      atomic.Uint64
	packHits         atomic.Uint64
	packStores       atomic.Uint64
	fanoutSubs       atomic.Uint64
	fanoutDetached   atomic.Uint64
	flightWaits      atomic.Uint64
}

func New(c config.Config, logger *slog.Logger) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	git, err := nativegit.New(c.GitBinary, c.MaxConcurrentGit)
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
	gcInterval, _ := time.ParseDuration(c.GCInterval)
	gcRetention, _ := time.ParseDuration(c.GCRetention)
	store, err := cache.Open(c.CacheDir, git, c.MaxCacheBytes, cache.RepackOptions{PackThreshold: c.RepackPackThreshold, Timeout: repackTimeout, GCInterval: gcInterval, GCRetention: gcRetention})
	if err != nil {
		return nil, err
	}
	store.SetPackCacheBytes(c.MaxPackCacheBytes)
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.ResponseHeaderTimeout = 30 * time.Second
	t.MaxConnsPerHost = c.MaxConcurrentRequests
	if logger == nil {
		logger = slog.Default()
	}
	s := &Server{
		config:    c,
		git:       git,
		store:     store,
		transport: t,
		logger:    logger,
		upstreams: make(map[string]*url.URL),
		requests:  newRequestLimiter(c.MaxConcurrentRequests, c.MaxRequestsPerSecond),
		responses: &responseBudget{limit: c.MaxTempResponseBytes},
	}
	s.gitTimeout, _ = time.ParseDuration(c.GitTimeout)
	s.fetchTimeout, _ = time.ParseDuration(c.FetchTimeout)
	s.authTimeout, _ = time.ParseDuration(c.UpstreamTimeout)
	s.writeIdleTimeout, _ = time.ParseDuration(c.WriteIdleTimeout)
	s.requestTimeout, _ = time.ParseDuration(c.RequestTimeout)
	s.probes.init(parseProbeTTL(c.AuthProbeCacheTTL))
	s.fanouts.init()
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
	s.client = &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	return s, nil
}

// Close must be called after the HTTP server has stopped handling requests.
func (s *Server) Close() error {
	s.transport.CloseIdleConnections()
	// Pumps stop and detach their subscribers, so in-flight transfers fall
	// back to their own file reads instead of truncating.
	s.fanouts.close()
	return s.store.Close()
}
