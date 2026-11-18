package config

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

type SSHUpstream struct {
	Address          string `yaml:"address" json:"address"`
	User             string `yaml:"user,omitempty" json:"user,omitempty"`
	KnownHosts       string `yaml:"known_hosts" json:"known_hosts"`
	PathPrefix       string `yaml:"path_prefix,omitempty" json:"path_prefix,omitempty"`
	CacheIdentityURL string `yaml:"cache_identity_url,omitempty" json:"cache_identity_url,omitempty"`
}

type SSH struct {
	Listen         string                 `yaml:"listen" json:"listen"`
	HostKey        string                 `yaml:"host_key" json:"host_key"`
	Mode           Mode                   `yaml:"mode" json:"mode"`
	AllowedUsers   []string               `yaml:"allowed_users" json:"allowed_users"`
	Upstream       *SSHUpstream           `yaml:"upstream,omitempty" json:"upstream,omitempty"`
	Upstreams      map[string]SSHUpstream `yaml:"upstreams,omitempty" json:"upstreams,omitempty"`
	MaxConnections int                    `yaml:"max_connections" json:"max_connections"`
}

func (s *SSH) Validate() error {
	if s.Listen == "" {
		s.Listen = "127.0.0.1:2222"
	}
	if _, _, err := net.SplitHostPort(s.Listen); err != nil {
		return fmt.Errorf("ssh.listen must be host:port")
	}
	if s.HostKey == "" {
		return fmt.Errorf("ssh.host_key is required")
	}
	key, err := ExpandHome(s.HostKey)
	if err != nil {
		return err
	}
	s.HostKey = key
	if s.Mode == "" {
		if s.Upstream != nil {
			s.Mode = ModeSingle
		} else {
			s.Mode = ModeMulti
		}
	}
	if len(s.AllowedUsers) == 0 {
		s.AllowedUsers = []string{"git"}
	}
	user := regexp.MustCompile(`^[A-Za-z0-9_.][A-Za-z0-9_.-]*$`)
	for _, name := range s.AllowedUsers {
		if !user.MatchString(name) {
			return fmt.Errorf("invalid ssh allowed user")
		}
	}
	if s.MaxConnections == 0 {
		s.MaxConnections = 16
	}
	if s.MaxConnections < 1 {
		return fmt.Errorf("ssh.max_connections must be positive")
	}
	upstreams := s.Upstreams
	switch s.Mode {
	case ModeSingle:
		if s.Upstream == nil || s.Upstreams != nil {
			return fmt.Errorf("ssh single mode requires upstream")
		}
		upstreams = map[string]SSHUpstream{SingleUpstreamID: *s.Upstream}
	case ModeMulti:
		if s.Upstream != nil || len(s.Upstreams) == 0 {
			return fmt.Errorf("ssh multi mode requires upstreams")
		}
	default:
		return fmt.Errorf("invalid ssh mode")
	}
	id := regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	for name, u := range upstreams {
		host, port, err := net.SplitHostPort(u.Address)
		n, parseErr := strconv.Atoi(port)
		if !id.MatchString(name) || err != nil || parseErr != nil || n < 1 || n > 65535 || host == "" || strings.ContainsAny(host, " /\\\r\n\x00") {
			return fmt.Errorf("invalid ssh upstream address")
		}
		if u.User != "" && !user.MatchString(u.User) {
			return fmt.Errorf("invalid ssh upstream user")
		}
		if u.KnownHosts == "" {
			return fmt.Errorf("ssh upstream known_hosts is required")
		}
		known, err := ExpandHome(u.KnownHosts)
		if err != nil {
			return err
		}
		u.KnownHosts = known
		if u.PathPrefix != "" {
			if err := ValidateRepositoryPath(u.PathPrefix); err != nil {
				return fmt.Errorf("invalid ssh path_prefix: %w", err)
			}
		}
		if u.CacheIdentityURL != "" {
			v, err := url.Parse(u.CacheIdentityURL)
			if err != nil || v.Host == "" || v.User != nil || v.RawQuery != "" || v.Fragment != "" || (v.Scheme != "https" && v.Scheme != "http") {
				return fmt.Errorf("invalid ssh cache_identity_url")
			}
		}
		if s.Mode == ModeSingle {
			s.Upstream = &u
		} else {
			s.Upstreams[name] = u
		}
	}
	return nil
}

func ValidateRepositoryPath(path string) error {
	if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "-") || strings.ContainsAny(path, "\\\x00\r\n'\"`;$|&<> ") {
		return fmt.Errorf("invalid repository path")
	}
	for _, p := range strings.Split(path, "/") {
		if p == "" || p == "." || p == ".." {
			return fmt.Errorf("invalid repository path component")
		}
	}
	return nil
}
