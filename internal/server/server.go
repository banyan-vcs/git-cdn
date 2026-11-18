package server

import (
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/cache"
	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/chiyutianyi/git-cdn/internal/nativegit"
)

type Server struct {
	config       config.Config
	git          *nativegit.Runner
	store        *cache.Store
	transport    *http.Transport
	client       *http.Client
	upstreams    map[string]*url.URL
	logger       *slog.Logger
	gitTimeout   time.Duration
	fetchTimeout time.Duration
	authTimeout  time.Duration
	local        atomic.Uint64
	hydrated     atomic.Uint64
	proxied      atomic.Uint64
	authDenied   atomic.Uint64
}

func New(c config.Config, logger *slog.Logger) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	git, err := nativegit.New(c.GitBinary, c.MaxConcurrentGit)
	if err != nil {
		return nil, err
	}
	store, err := cache.Open(c.CacheDir, git, c.MaxCacheBytes)
	if err != nil {
		return nil, err
	}
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.ResponseHeaderTimeout = 30 * time.Second
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
	}
	s.gitTimeout, _ = time.ParseDuration(c.GitTimeout)
	s.fetchTimeout, _ = time.ParseDuration(c.FetchTimeout)
	s.authTimeout, _ = time.ParseDuration(c.UpstreamTimeout)
	s.client = &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
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
	return s, nil
}

// Close must be called after the HTTP server has stopped handling requests.
func (s *Server) Close() error {
	s.transport.CloseIdleConnections()
	return s.store.Close()
}
