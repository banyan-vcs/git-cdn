package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTransportAndResourceSecurityValidation(t *testing.T) {
	for _, test := range []struct {
		name   string
		modify func(*Config)
		valid  bool
	}{
		{"loopback HTTP", func(c *Config) {}, true},
		{"IPv6 loopback HTTP", func(c *Config) { c.Listen = "[::1]:8080" }, true},
		{"public HTTP", func(c *Config) { c.Listen = ":8080" }, false},
		{"TLS listener", func(c *Config) { c.Listen = ":8080"; c.TLSCert = "cert"; c.TLSKey = "key" }, true},
		{"trusted proxy", func(c *Config) { c.Listen = ":8080"; c.TrustedProxyCIDRs = []string{"10.0.0.0/8"} }, true},
		{"invalid proxy CIDR", func(c *Config) { c.TrustedProxyCIDRs = []string{"proxy"} }, false},
		{"universal proxy trust", func(c *Config) { c.TrustedProxyCIDRs = []string{"0.0.0.0/0"} }, false},
		{"explicit trusted plaintext", func(c *Config) { c.Listen = ":8080"; c.AllowInsecureHTTP = true }, true},
		{"remote plaintext upstream", func(c *Config) { c.Upstream.URL = "http://origin.example" }, false},
		{"loopback plaintext upstream", func(c *Config) { c.Upstream.URL = "http://127.0.0.1:8081" }, true},
		{"explicit plaintext upstream", func(c *Config) { c.Upstream.URL = "http://origin.example"; c.AllowInsecureHTTP = true }, true},
		{"forged identity forwarding", func(c *Config) { c.AuthHeaders = []string{"X-Forwarded-User"} }, false},
		{"forged remote identity", func(c *Config) { c.AuthHeaders = []string{"X-Remote-User"} }, false},
		{"request concurrency", func(c *Config) { c.MaxConcurrentRequests = -1 }, false},
		{"request rate", func(c *Config) { c.MaxRequestsPerSecond = -1 }, false},
		{"temporary response capacity", func(c *Config) { c.MaxTempResponseBytes = -1 }, false},
		{"write idle timeout", func(c *Config) { c.WriteIdleTimeout = "0s" }, false},
		{"total request timeout", func(c *Config) { c.RequestTimeout = "-1s" }, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Config{Upstream: &Upstream{URL: "https://origin.example"}}
			test.modify(&c)
			{
				err := c.Validate()
				require.EqualValues(t, test.valid, (err == nil), "valid=%v error=%v", test.valid, err)
			}
		})
	}
}

func TestMultiSiteCredentialsRequireDistinctHosts(t *testing.T) {
	for _, test := range []struct {
		name, first, second string
		shared, valid       bool
	}{
		{"missing hosts", "", "", false, false},
		{"distinct hosts", "one.cdn.example", "two.cdn.example", false, true},
		{"same host", "cdn.example", "CDN.EXAMPLE", false, false},
		{"same hostname with default port", "cdn.example", "cdn.example:443", false, false},
		{"URL instead of host", "https://one.cdn.example", "two.cdn.example", false, false},
		{"host with path", "one.cdn.example/path", "two.cdn.example", false, false},
		{"explicit path-scoped shared domain", "", "", true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			c := Config{AllowSharedCredentialHost: test.shared, Upstreams: map[string]Upstream{
				"one": {URL: "https://one.example", CredentialHost: test.first},
				"two": {URL: "https://two.example", CredentialHost: test.second},
			}}
			{
				err := c.Validate()
				require.EqualValues(t, test.valid, (err == nil), "valid=%v error=%v", test.valid, err)
			}
		})
	}
}
