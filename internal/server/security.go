package server

import (
	"net"
	"net/http"
	"strings"
)

func (s *Server) secureRequest(r *http.Request) bool {
	if r.TLS != nil || s.config.AllowInsecureHTTP {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	if ip.IsLoopback() {
		return true
	}
	protocols := r.Header.Values("X-Forwarded-Proto")
	if len(protocols) != 1 || protocols[0] != "https" {
		return false
	}
	for _, proxy := range s.trustedProxies {
		if proxy.Contains(ip) {
			return true
		}
	}
	return false
}

func (s *Server) credentialHostMatches(r *http.Request, rt route) bool {
	upstream := s.config.Upstreams[rt.id]
	if s.config.Upstream != nil {
		upstream = *s.config.Upstream
	}
	if upstream.CredentialHost == "" || strings.EqualFold(r.Host, upstream.CredentialHost) {
		return true
	}
	for _, header := range s.config.AuthHeaders {
		if len(r.Header.Values(header)) > 0 {
			return false
		}
	}
	return true
}
