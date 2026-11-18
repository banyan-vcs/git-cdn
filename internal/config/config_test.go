package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestUnsafeUpstreamsAndAuthHeadersAreRejected(t *testing.T) {
	for _, u := range []string{"file:///tmp/repo", "https://user:secret@example.com", "https://example.com?token=secret", "https://example.com#fragment", "example.com"} {
		c := Config{Upstreams: map[string]Upstream{"origin": {URL: u}}}
		require.Error(t, c.Validate(), "invalid origin accepted: %s", u)
	}
	for _, headers := range [][]string{{"Host"}, {"Git-Protocol"}, {"Authorization", "authorization"}, {"X-Auth\r\nHeader"}, {"Proxy-Authorization"}} {
		c := Config{Upstreams: map[string]Upstream{"origin": {URL: "https://example.com"}}, AuthHeaders: headers}
		require.Error(t, c.Validate(), "invalid auth headers accepted: %v", headers)
	}
}

func loadYAML(t *testing.T, body string) (Config, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	return Load(path)
}

func TestYAMLModeLoadingAndDefaults(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	for _, test := range []struct {
		name string
		body string
		mode Mode
	}{
		{"single", "mode: single\nupstream:\n  url: https://gitlab.com\n", ModeSingle},
		{"multi", "mode: multi\nupstreams:\n  gitlab:\n    url: https://gitlab.com\n  github:\n    url: https://github.com\n", ModeMulti},
		{"inferred single", "upstream:\n  url: https://gitlab.com\n", ModeSingle},
		{"inferred multi with one site", "upstreams:\n  gitlab:\n    url: https://gitlab.com\n", ModeMulti},
	} {
		t.Run(test.name, func(t *testing.T) {
			c, err := loadYAML(t, "# Configuration supports comments\n"+test.body)
			require.NoError(t, err)
			require.EqualValues(t, test.mode, c.Mode, "unexpected mode or defaults: %+v", c)
			require.EqualValues(t, DefaultMaxCacheBytes, c.MaxCacheBytes, "unexpected mode or defaults: %+v", c)
			require.EqualValues(t, filepath.Join(home, ".git-cdn", "caches"), c.CacheDir, "unexpected mode or defaults: %+v", c)
			require.EqualValues(t, "127.0.0.1:8080", c.Listen, "unexpected mode or defaults: %+v", c)
			require.EqualValues(t, "2m", c.FetchTimeout, "unexpected mode or defaults: %+v", c)
			require.EqualValues(t, 8<<20, c.MaxRequestBytes, "unexpected mode or defaults: %+v", c)
			require.Len(t, c.AuthHeaders, 1, "unexpected mode or defaults: %+v", c)
			require.EqualValues(t, "Authorization", c.AuthHeaders[0], "unexpected mode or defaults: %+v", c)
		})
	}
}

func TestHomePathsAndCacheOverrides(t *testing.T) {
	home, err := os.UserHomeDir()
	require.NoError(t, err)
	for _, test := range []struct{ input, expected string }{
		{DefaultConfigPath, filepath.Join(home, ".git-cdn", "config.yaml")},
		{DefaultCacheDir, filepath.Join(home, ".git-cdn", "caches")},
		{"~", home},
		{"~/", home},
		{"./config.yaml", "./config.yaml"},
		{"/data/caches", "/data/caches"},
	} {
		actual, err := ExpandHome(test.input)
		require.NoError(t, err, "ExpandHome(%q) = %q, %v", test.input, actual, err)
		require.EqualValues(t, test.expected, actual, "ExpandHome(%q) = %q, %v", test.input, actual, err)
	}
	for _, path := range []string{"~another-user/caches", "~another-user"} {
		{
			_, err := ExpandHome(path)
			require.Error(t, err, "unsupported home prefix accepted: %s", path)
		}
		{
			_, err := Load(path)
			require.Error(t, err, "Load did not expand its input: %v", err)
			require.Contains(t, err.Error(), "unsupported home prefix", "Load did not expand its input: %v", err)
		}
	}
	for _, test := range []struct{ setting, expected string }{
		{"~/custom/git-cdn", filepath.Join(home, "custom", "git-cdn")},
		{"/data/git-cdn/caches", "/data/git-cdn/caches"},
		{"./local-cache", "./local-cache"},
	} {
		c, err := loadYAML(t, "mode: single\nupstream:\n  url: https://example.com\ncache_dir: "+test.setting+"\n")
		require.NoError(t, err, "cache override %q: %q %v", test.setting, c.CacheDir, err)
		require.EqualValues(t, test.expected, c.CacheDir, "cache override %q: %q %v", test.setting, c.CacheDir, err)
	}
}

func TestYAMLRejectsAmbiguousAndMalformedConfiguration(t *testing.T) {
	valid := "upstream:\n  url: https://gitlab.com\n"
	for _, test := range []struct{ name, body string }{
		{"unknown mode", "mode: automatic\n" + valid},
		{"single with upstreams", "mode: single\nupstreams:\n  gitlab:\n    url: https://gitlab.com\n"},
		{"multi with upstream", "mode: multi\n" + valid},
		{"both", valid + "upstreams:\n  gitlab:\n    url: https://gitlab.com\n"},
		{"missing upstream", "mode: single\n"},
		{"empty upstreams", "mode: multi\nupstreams: {}\n"},
		{"unknown field", valid + "git_timout: 3m\n"},
		{"unknown nested field", "upstream:\n  url: https://gitlab.com\n  timeout: 3m\n"},
		{"duplicate field", "mode: single\nmode: multi\n" + valid},
		{"multiple documents", valid + "---\n" + valid},
		{"malformed yaml", "upstream: [\n"},
		{"empty document", "# empty\n"},
		{"invalid timeout", valid + "fetch_timeout: forever\n"},
		{"invalid size", valid + "max_request_bytes: -1\n"},
		{"invalid cache size", valid + "max_cache_bytes: -1\n"},
		{"TLS pair missing", valid + "tls_cert: server.pem\n"},
		{"unsafe single URL", "mode: single\nupstream:\n  url: https://user:secret@example.com\n"},
		{"invalid site ID", "mode: multi\nupstreams:\n  bad/id:\n    url: https://example.com\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			{
				_, err := loadYAML(t, test.body)
				require.Error(t, err, "invalid configuration accepted")
			}
		})
	}
	{
		_, err := loadYAML(t, valid+"#"+strings.Repeat(" ", 1<<20))
		require.Error(t, err, "oversized configuration accepted")
	}
}

func TestExampleYAMLConfigurations(t *testing.T) {
	for _, test := range []struct {
		path string
		mode Mode
	}{
		{"../../config.example.yaml", ModeSingle},
		{"../../config.multi.example.yaml", ModeMulti},
	} {
		c, err := Load(test.path)
		require.NoError(t, err, "example %s: mode=%s error=%v", test.path, c.Mode, err)
		require.EqualValues(t, test.mode, c.Mode, "example %s: mode=%s error=%v", test.path, c.Mode, err)
	}
}
