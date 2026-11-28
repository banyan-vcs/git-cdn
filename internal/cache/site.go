package cache

import (
	"os"
	"path/filepath"
	"strings"
)

// A scope's site is the upstream alias it was last served for — the segment
// that selected the upstream in multi-site mode, or the single upstream in
// single-site mode. It is recorded beside the scope so that a restart keeps
// the per-site split of the overview.

// UnknownSite is reported for scopes whose site is not recorded: they were
// cached before sites were recorded and have not been served since.
const UnknownSite = "unknown"

// siteFile names the record of a scope's site, beside its recency stamp.
const siteFile = ".site"

// persistSite writes a scope's site record. A record that cannot be written
// costs the scope its site until it is served again; the bytes are still
// counted, under the unknown site.
func (s *Store) persistSite(key, site string) {
	_ = atomicFile(filepath.Join(s.Root, "repos", key), siteFile, []byte(site))
}

func (s *Store) loadSite(path string) string {
	data, err := os.ReadFile(filepath.Join(path, siteFile))
	if err != nil {
		return ""
	}
	return recordedSite(data)
}

// recordedSite reads back a scope's site. Anything that is not a site alias
// counts as none.
func recordedSite(data []byte) string {
	site := strings.TrimSpace(string(data))
	if site == "" || len(site) > 128 || strings.ContainsFunc(site, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == ':')
	}) {
		return ""
	}
	return site
}
