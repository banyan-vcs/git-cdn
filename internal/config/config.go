package config

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Mode string

const (
	ModeSingle           Mode  = "single"
	ModeMulti            Mode  = "multi"
	SingleUpstreamID           = "default"
	DefaultConfigPath          = "~/.git-cdn/config.yaml"
	DefaultCacheDir            = "~/.git-cdn/caches"
	DefaultMaxCacheBytes int64 = 20 << 30
	// DefaultMaxPackCacheBytes bounds the verbatim response-pack cache per
	// repository scope. A negative configured value disables the feature.
	DefaultMaxPackCacheBytes int64 = 1 << 30
)

// ExpandHome resolves the current user's home prefix without expanding other users or variables.
func ExpandHome(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve user home directory: %w", err)
		}
		if path == "~" {
			return home, nil
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("unsupported home prefix: use ~ or ~/")
	}
	return path, nil
}

type Upstream struct {
	URL            string `json:"url" yaml:"url"`
	CredentialHost string `json:"credential_host,omitempty" yaml:"credential_host,omitempty"`
}

type Config struct {
	Mode                      Mode                `json:"mode" yaml:"mode"`
	Listen                    string              `json:"listen" yaml:"listen"`
	CacheDir                  string              `json:"cache_dir" yaml:"cache_dir"`
	GitBinary                 string              `json:"git_binary" yaml:"git_binary"`
	Upstream                  *Upstream           `json:"upstream,omitempty" yaml:"upstream,omitempty"`
	Upstreams                 map[string]Upstream `json:"upstreams,omitempty" yaml:"upstreams,omitempty"`
	AuthHeaders               []string            `json:"auth_headers" yaml:"auth_headers"`
	MaxRequestBytes           int64               `json:"max_request_bytes" yaml:"max_request_bytes"`
	MaxRefsBytes              int64               `json:"max_refs_bytes" yaml:"max_refs_bytes"`
	MaxResponseBytes          int64               `json:"max_response_bytes" yaml:"max_response_bytes"`
	MaxCacheBytes             int64               `json:"max_cache_bytes" yaml:"max_cache_bytes"`
	RepackPackThreshold       int                 `json:"repack_pack_threshold" yaml:"repack_pack_threshold"`
	RepackTimeout             string              `json:"repack_timeout" yaml:"repack_timeout"`
	GCInterval                string              `json:"gc_interval" yaml:"gc_interval"`
	GCRetention               string              `json:"gc_retention" yaml:"gc_retention"`
	MaxConcurrentGit          int                 `json:"max_concurrent_git" yaml:"max_concurrent_git"`
	MaxConcurrentRequests     int                 `json:"max_concurrent_requests" yaml:"max_concurrent_requests"`
	MaxRequestsPerSecond      int                 `json:"max_requests_per_second" yaml:"max_requests_per_second"`
	MaxTempResponseBytes      int64               `json:"max_temp_response_bytes" yaml:"max_temp_response_bytes"`
	MaxPackCacheBytes         int64               `json:"max_pack_cache_bytes" yaml:"max_pack_cache_bytes"`
	AuthProbeCacheTTL         string              `json:"auth_probe_cache_ttl" yaml:"auth_probe_cache_ttl"`
	WriteIdleTimeout          string              `json:"write_idle_timeout" yaml:"write_idle_timeout"`
	RequestTimeout            string              `json:"request_timeout" yaml:"request_timeout"`
	AllowInsecureHTTP         bool                `json:"allow_insecure_http" yaml:"allow_insecure_http"`
	TrustedProxyCIDRs         []string            `json:"trusted_proxy_cidrs" yaml:"trusted_proxy_cidrs"`
	AllowSharedCredentialHost bool                `json:"allow_shared_credential_host" yaml:"allow_shared_credential_host"`
	FetchTimeout              string              `json:"fetch_timeout" yaml:"fetch_timeout"`
	GitTimeout                string              `json:"git_timeout" yaml:"git_timeout"`
	UpstreamTimeout           string              `json:"upstream_timeout" yaml:"upstream_timeout"`
	TLSCert                   string              `json:"tls_cert" yaml:"tls_cert"`
	TLSKey                    string              `json:"tls_key" yaml:"tls_key"`
	SSH                       *SSH                `json:"ssh,omitempty" yaml:"ssh,omitempty"`
}

func Load(path string) (Config, error) {
	path, err := ExpandHome(path)
	if err != nil {
		return Config{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	const maxSize = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxSize+1))
	if err != nil {
		return Config{}, err
	}
	if len(data) > maxSize {
		return Config{}, fmt.Errorf("configuration exceeds 1 MiB")
	}
	var c Config
	d := yaml.NewDecoder(bytes.NewReader(data))
	d.KnownFields(true)
	if err := d.Decode(&c); err != nil {
		return c, err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return c, fmt.Errorf("configuration must contain one YAML document")
	}
	return c, c.Validate()
}

func (c *Config) Validate() error {
	if c.Listen == "" {
		c.Listen = "127.0.0.1:8080"
	}
	if c.CacheDir == "" {
		c.CacheDir = DefaultCacheDir
	}
	cacheDir, err := ExpandHome(c.CacheDir)
	if err != nil {
		return fmt.Errorf("cache_dir: %w", err)
	}
	c.CacheDir = cacheDir
	if c.GitBinary == "" {
		c.GitBinary = "git"
	}
	if c.Mode == "" {
		if c.Upstream != nil {
			c.Mode = ModeSingle
		} else {
			c.Mode = ModeMulti
		}
	}
	upstreams := c.Upstreams
	switch c.Mode {
	case ModeSingle:
		if c.Upstream == nil || len(c.Upstreams) > 0 {
			return fmt.Errorf("single mode requires upstream and excludes upstreams")
		}
		upstreams = map[string]Upstream{SingleUpstreamID: *c.Upstream}
	case ModeMulti:
		if c.Upstream != nil || len(c.Upstreams) == 0 {
			return fmt.Errorf("multi mode requires upstreams and excludes upstream")
		}
	default:
		return fmt.Errorf("mode must be single or multi")
	}
	id := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for name, upstream := range upstreams {
		u, err := url.Parse(upstream.URL)
		if !id.MatchString(name) || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(u.Path, "\r\n") {
			return fmt.Errorf("invalid upstream %q: use a fixed HTTP(S) base URL without credentials, query or fragment", name)
		}
		if u.Scheme == "http" && !c.AllowInsecureHTTP && !IsLoopbackHost(u.Hostname()) {
			return fmt.Errorf("upstream %q requires HTTPS; allow_insecure_http is only for trusted plaintext networks", name)
		}
	}
	if err := c.validateAuthHeaders(); err != nil {
		return err
	}
	if err := c.validateLimits(); err != nil {
		return err
	}
	if err := c.validateTransport(); err != nil {
		return err
	}
	if err := c.validateCredentialHosts(upstreams); err != nil {
		return err
	}
	if c.SSH != nil {
		return c.SSH.Validate()
	}
	return nil
}

func (c *Config) validateAuthHeaders() error {
	if c.AuthHeaders == nil {
		c.AuthHeaders = []string{"Authorization"}
	}
	header := regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	seen := make(map[string]bool)
	for _, name := range c.AuthHeaders {
		if !header.MatchString(name) {
			return fmt.Errorf("invalid authentication header %q", name)
		}
		key := strings.ToLower(name)
		if seen[key] {
			return fmt.Errorf("duplicate authentication header %q", name)
		}
		seen[key] = true
		if strings.HasPrefix(key, "x-forwarded-") {
			return fmt.Errorf("proxy identity header %q cannot be used as a client credential", name)
		}
		switch key {
		case "host", "connection", "content-length", "content-type", "content-encoding", "git-protocol", "proxy-authorization", "transfer-encoding", "upgrade", "forwarded", "x-real-ip", "x-remote-user", "remote-user", "x-authenticated-user":
			return fmt.Errorf("reserved authentication header %q", name)
		}
	}
	return nil
}

func (c *Config) validateLimits() error {
	for _, limit := range []struct {
		p *int64
		d int64
	}{{&c.MaxRequestBytes, 8 << 20}, {&c.MaxRefsBytes, 16 << 20}, {&c.MaxResponseBytes, 1 << 30}, {&c.MaxCacheBytes, DefaultMaxCacheBytes}, {&c.MaxTempResponseBytes, 4 << 30}} {
		if *limit.p == 0 {
			*limit.p = limit.d
		}
		if *limit.p < 1 {
			return fmt.Errorf("byte limits must be positive")
		}
	}
	if c.MaxPackCacheBytes == 0 {
		c.MaxPackCacheBytes = DefaultMaxPackCacheBytes
	}
	if c.MaxPackCacheBytes < 0 {
		c.MaxPackCacheBytes = 0
	}
	if c.MaxConcurrentGit == 0 {
		c.MaxConcurrentGit = 4
	}
	if c.MaxConcurrentGit < 1 {
		return fmt.Errorf("max_concurrent_git must be positive")
	}
	if c.MaxConcurrentRequests == 0 {
		c.MaxConcurrentRequests = 16
	}
	if c.MaxRequestsPerSecond == 0 {
		c.MaxRequestsPerSecond = 64
	}
	if c.MaxConcurrentRequests < 1 || c.MaxRequestsPerSecond < 1 {
		return fmt.Errorf("request concurrency and rate limits must be positive")
	}
	for _, timeout := range []struct {
		p *string
		d string
	}{{&c.FetchTimeout, "2m"}, {&c.GitTimeout, "2m"}, {&c.UpstreamTimeout, "30s"}, {&c.WriteIdleTimeout, "30s"}, {&c.RequestTimeout, "10m"}, {&c.RepackTimeout, "2m"}, {&c.GCInterval, "24h"}, {&c.GCRetention, "336h"}} {
		if *timeout.p == "" {
			*timeout.p = timeout.d
		}
		v, err := time.ParseDuration(*timeout.p)
		if err != nil || v <= 0 {
			return fmt.Errorf("timeouts must be positive Go durations")
		}
	}
	if c.AuthProbeCacheTTL == "" {
		c.AuthProbeCacheTTL = "0s"
	}
	if ttl, err := time.ParseDuration(c.AuthProbeCacheTTL); err != nil || ttl < 0 {
		return fmt.Errorf("auth_probe_cache_ttl must be a non-negative Go duration")
	}
	if c.RepackPackThreshold == 0 {
		c.RepackPackThreshold = 32
	}
	if c.RepackPackThreshold < 2 {
		return fmt.Errorf("repack_pack_threshold must be at least 2")
	}
	retention, _ := time.ParseDuration(c.GCRetention)
	if retention < 14*24*time.Hour {
		return fmt.Errorf("gc_retention must be at least 336h (14 days)")
	}
	return nil
}

func (c *Config) validateTransport() error {
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("tls_cert and tls_key must be configured together")
	}
	for _, cidr := range c.TrustedProxyCIDRs {
		_, network, err := net.ParseCIDR(cidr)
		if err != nil {
			return fmt.Errorf("invalid trusted proxy CIDR %q", cidr)
		}
		if prefix, _ := network.Mask.Size(); prefix == 0 {
			return fmt.Errorf("trusted_proxy_cidrs cannot trust all addresses")
		}
	}
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Errorf("listen must be a TCP host:port: %w", err)
	}
	if c.TLSCert == "" && !c.AllowInsecureHTTP && !IsLoopbackHost(host) && len(c.TrustedProxyCIDRs) == 0 {
		return fmt.Errorf("non-loopback HTTP listener requires TLS, trusted_proxy_cidrs, or explicit allow_insecure_http")
	}
	return nil
}

func (c *Config) validateCredentialHosts(upstreams map[string]Upstream) error {
	credentialHosts := make(map[string]bool)
	for name, upstream := range upstreams {
		if upstream.CredentialHost == "" {
			if len(upstreams) > 1 && len(c.AuthHeaders) > 0 && !c.AllowSharedCredentialHost {
				return fmt.Errorf("upstream %q requires a distinct credential_host; shared domains require explicit allow_shared_credential_host", name)
			}
			continue
		}
		u, err := url.Parse("https://" + upstream.CredentialHost)
		if err != nil || u.Host != upstream.CredentialHost || u.Hostname() == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("invalid credential_host for upstream %q", name)
		}
		key := strings.ToLower(strings.TrimSuffix(u.Hostname(), "."))
		if credentialHosts[key] {
			return fmt.Errorf("credential_host must be unique across upstreams")
		}
		credentialHosts[key] = true
	}
	return nil
}

// IsLoopbackHost permits plaintext development traffic without resolving remote DNS names.
func IsLoopbackHost(host string) bool {
	return strings.EqualFold(host, "localhost") || net.ParseIP(host).IsLoopback()
}
