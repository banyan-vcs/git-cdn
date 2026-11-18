package config

import (
	"bytes"
	"fmt"
	"io"
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
	ModeSingle        Mode = "single"
	ModeMulti         Mode = "multi"
	SingleUpstreamID       = "default"
	DefaultConfigPath      = "~/.git-cdn/config.yaml"
	DefaultCacheDir        = "~/.git-cdn/caches"
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
	URL string `json:"url" yaml:"url"`
}

type Config struct {
	Mode             Mode                `json:"mode" yaml:"mode"`
	Listen           string              `json:"listen" yaml:"listen"`
	CacheDir         string              `json:"cache_dir" yaml:"cache_dir"`
	GitBinary        string              `json:"git_binary" yaml:"git_binary"`
	Upstream         *Upstream           `json:"upstream,omitempty" yaml:"upstream,omitempty"`
	Upstreams        map[string]Upstream `json:"upstreams,omitempty" yaml:"upstreams,omitempty"`
	AuthHeaders      []string            `json:"auth_headers" yaml:"auth_headers"`
	MaxRequestBytes  int64               `json:"max_request_bytes" yaml:"max_request_bytes"`
	MaxRefsBytes     int64               `json:"max_refs_bytes" yaml:"max_refs_bytes"`
	MaxResponseBytes int64               `json:"max_response_bytes" yaml:"max_response_bytes"`
	MaxConcurrentGit int                 `json:"max_concurrent_git" yaml:"max_concurrent_git"`
	FetchTimeout     string              `json:"fetch_timeout" yaml:"fetch_timeout"`
	GitTimeout       string              `json:"git_timeout" yaml:"git_timeout"`
	UpstreamTimeout  string              `json:"upstream_timeout" yaml:"upstream_timeout"`
	TLSCert          string              `json:"tls_cert" yaml:"tls_cert"`
	TLSKey           string              `json:"tls_key" yaml:"tls_key"`
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
	}
	if c.AuthHeaders == nil {
		c.AuthHeaders = []string{"Authorization"}
	}
	header := regexp.MustCompile(`^[A-Za-z0-9-]+$`)
	seen := make(map[string]bool)
	for _, name := range c.AuthHeaders {
		if !header.MatchString(name) {
			return fmt.Errorf("invalid authentication header %q", name)
		}
		if seen[strings.ToLower(name)] {
			return fmt.Errorf("duplicate authentication header %q", name)
		}
		seen[strings.ToLower(name)] = true
		switch strings.ToLower(name) {
		case "host", "connection", "content-length", "content-type", "content-encoding", "git-protocol", "proxy-authorization", "transfer-encoding", "upgrade", "forwarded":
			return fmt.Errorf("reserved authentication header %q", name)
		}
	}
	for _, limit := range []struct {
		p *int64
		d int64
	}{{&c.MaxRequestBytes, 8 << 20}, {&c.MaxRefsBytes, 16 << 20}, {&c.MaxResponseBytes, 1 << 30}} {
		if *limit.p == 0 {
			*limit.p = limit.d
		}
		if *limit.p < 1 {
			return fmt.Errorf("byte limits must be positive")
		}
	}
	if c.MaxConcurrentGit == 0 {
		c.MaxConcurrentGit = 4
	}
	if c.MaxConcurrentGit < 1 {
		return fmt.Errorf("max_concurrent_git must be positive")
	}
	for _, timeout := range []struct {
		p *string
		d string
	}{{&c.FetchTimeout, "2m"}, {&c.GitTimeout, "2m"}, {&c.UpstreamTimeout, "30s"}} {
		if *timeout.p == "" {
			*timeout.p = timeout.d
		}
		v, err := time.ParseDuration(*timeout.p)
		if err != nil || v <= 0 {
			return fmt.Errorf("timeouts must be positive Go durations")
		}
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return fmt.Errorf("tls_cert and tls_key must be configured together")
	}
	return nil
}
