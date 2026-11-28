package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/buildinfo"
	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
)

// Draining reports whether the node rejects new work. In-flight transfers
// keep running; both frontends consult this before admitting a request.
func (s *Server) Draining() bool { return s.drain.Load() }

// SetDraining switches the node's admission. Load balancers probing
// /healthz stop routing to a draining node on their own.
func (s *Server) SetDraining(draining bool) { s.drain.Store(draining) }

// SetConfigPath records where the running configuration came from, so the
// reload endpoint reads the same file again.
func (s *Server) SetConfigPath(path string) { s.configPath = path }

// SetSSHSessionCounter registers the SSH frontend's live-connection count
// for the status endpoint. Without an SSH frontend the count stays zero.
func (s *Server) SetSSHSessionCounter(count func() int) { s.sshSessions = count }

func (s *Server) sshSessionCount() int {
	if s.sshSessions == nil {
		return 0
	}
	return s.sshSessions()
}

// ConfigChange names one setting a reload observed, with its values
// before and after.
type ConfigChange struct {
	Field string `json:"field"`
	Old   string `json:"old"`
	New   string `json:"new"`
}

// ApplyConfig moves the reloadable subset of next onto the running node
// and reports what changed plus what still needs a restart. next must
// already be validated. Comparisons run against the live components, not
// the startup file, so repeated reloads diff correctly.
func (s *Server) ApplyConfig(next config.Config) (applied, restart []ConfigChange, err error) {
	change := func(field string, old, new any) ConfigChange {
		return ConfigChange{Field: field, Old: formatValue(old), New: formatValue(new)}
	}
	if stats := s.store.Stats(); next.MaxCacheBytes != stats.MaxBytes {
		if err := s.store.SetMaxBytes(next.MaxCacheBytes); err != nil {
			return nil, nil, err
		}
		applied = append(applied, change("max_cache_bytes", stats.MaxBytes, next.MaxCacheBytes))
	}
	if current := s.store.PackCacheBytes(); next.MaxPackCacheBytes != current {
		s.store.SetPackCacheBytes(next.MaxPackCacheBytes)
		applied = append(applied, change("max_pack_cache_bytes", current, next.MaxPackCacheBytes))
	}
	if current := s.responses.currentLimit(); next.MaxTempResponseBytes != current {
		s.responses.setLimit(next.MaxTempResponseBytes)
		applied = append(applied, change("max_temp_response_bytes", current, next.MaxTempResponseBytes))
	}
	if concurrency, rate := s.requests.Load().settings(); next.MaxConcurrentRequests != concurrency || next.MaxRequestsPerSecond != rate {
		// A whole fresh limiter: the token bucket restarts full and
		// in-flight requests finish on the one they acquired.
		s.requests.Store(newRequestLimiter(next.MaxConcurrentRequests, next.MaxRequestsPerSecond))
		applied = append(applied, change("max_concurrent_requests", concurrency, next.MaxConcurrentRequests))
		if next.MaxRequestsPerSecond != rate {
			applied = append(applied, change("max_requests_per_second", rate, next.MaxRequestsPerSecond))
		}
	}
	tune := newTuning(next)
	if current := s.tune.Load(); *current != *tune {
		if tune.MaxRequestBytes != current.MaxRequestBytes {
			applied = append(applied, change("max_request_bytes", current.MaxRequestBytes, tune.MaxRequestBytes))
		}
		if tune.MaxRefsBytes != current.MaxRefsBytes {
			applied = append(applied, change("max_refs_bytes", current.MaxRefsBytes, tune.MaxRefsBytes))
		}
		if tune.GitTimeout != current.GitTimeout {
			applied = append(applied, change("git_timeout", current.GitTimeout, tune.GitTimeout))
		}
		if tune.FetchTimeout != current.FetchTimeout {
			applied = append(applied, change("fetch_timeout", current.FetchTimeout, tune.FetchTimeout))
		}
		if tune.AuthTimeout != current.AuthTimeout {
			applied = append(applied, change("upstream_timeout", current.AuthTimeout, tune.AuthTimeout))
		}
		if tune.WriteIdleTimeout != current.WriteIdleTimeout {
			applied = append(applied, change("write_idle_timeout", current.WriteIdleTimeout, tune.WriteIdleTimeout))
		}
		if tune.RequestTimeout != current.RequestTimeout {
			applied = append(applied, change("request_timeout", current.RequestTimeout, tune.RequestTimeout))
		}
		s.tune.Store(tune)
	}
	if current, parsed := s.probes.TTL(), parseProbeTTL(next.AuthProbeCacheTTL); parsed != current {
		s.probes.setTTL(parsed)
		applied = append(applied, change("auth_probe_cache_ttl", current, parsed))
	}
	return applied, s.restartChanges(next), nil
}

// restartChanges reports settings fixed at construction that differ from
// the file. The list is a pending-restart indicator: it keeps reporting a
// difference until the node restarts into it, because the listener
// wiring, upstream routing, SSH frontend and maintenance tuning all carry
// state a live swap would corrupt.
func (s *Server) restartChanges(next config.Config) []ConfigChange {
	old := s.config
	var changes []ConfigChange
	changed := func(field string, a, b any) {
		if !reflect.DeepEqual(a, b) {
			changes = append(changes, ConfigChange{Field: field, Old: formatValue(a), New: formatValue(b)})
		}
	}
	changed("mode", old.Mode, next.Mode)
	changed("listen", old.Listen, next.Listen)
	changed("cache_dir", old.CacheDir, next.CacheDir)
	changed("git_binary", old.GitBinary, next.GitBinary)
	changed("upstream", old.Upstream, next.Upstream)
	changed("upstreams", old.Upstreams, next.Upstreams)
	changed("auth_headers", old.AuthHeaders, next.AuthHeaders)
	changed("max_response_bytes", old.MaxResponseBytes, next.MaxResponseBytes)
	changed("repack_pack_threshold", old.RepackPackThreshold, next.RepackPackThreshold)
	changed("repack_timeout", old.RepackTimeout, next.RepackTimeout)
	changed("gc_interval", old.GCInterval, next.GCInterval)
	changed("gc_retention", old.GCRetention, next.GCRetention)
	changed("gc_timeout", old.GCTimeout, next.GCTimeout)
	changed("max_concurrent_git", old.MaxConcurrentGit, next.MaxConcurrentGit)
	changed("memory_limit_bytes", old.MemoryLimitBytes, next.MemoryLimitBytes)
	changed("allow_insecure_http", old.AllowInsecureHTTP, next.AllowInsecureHTTP)
	changed("trusted_proxy_cidrs", old.TrustedProxyCIDRs, next.TrustedProxyCIDRs)
	changed("allow_shared_credential_host", old.AllowSharedCredentialHost, next.AllowSharedCredentialHost)
	changed("upstream_proxy", old.UpstreamProxy, next.UpstreamProxy)
	changed("upstream_ca_file", old.UpstreamCAFile, next.UpstreamCAFile)
	changed("tls_cert", old.TLSCert, next.TLSCert)
	changed("tls_key", old.TLSKey, next.TLSKey)
	changed("admin_listen", old.AdminListen, next.AdminListen)
	changed("ssh", old.SSH, next.SSH)
	return changes
}

func formatValue(v any) string {
	if v == nil {
		return "unset"
	}
	value := reflect.ValueOf(v)
	if value.Kind() == reflect.Pointer {
		if value.IsNil() {
			return "unset"
		}
		value = value.Elem()
	}
	if value.Kind() == reflect.String && value.String() == "" {
		return "unset"
	}
	return fmt.Sprintf("%v", value.Interface())
}

// AdminHandler serves the node operations API: live status, the cache
// scopes, drain control and configuration reload. It belongs on the
// loopback-only admin listener and carries no authentication — the
// configuration rejects a non-loopback admin_listen for exactly that
// reason.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", s.adminStatus)
	mux.HandleFunc("GET /scopes", s.adminScopes)
	mux.HandleFunc("POST /drain", s.adminDrain)
	mux.HandleFunc("POST /undrain", s.adminUndrain)
	mux.HandleFunc("POST /reload", s.adminReload)
	return mux
}

type adminCache struct {
	Entries       int   `json:"entries"`
	Bytes         int64 `json:"bytes"`
	MaxBytes      int64 `json:"max_bytes"`
	ReservedBytes int64 `json:"reserved_bytes"`
}

type adminTotals struct {
	LocalRequests        uint64 `json:"local_requests"`
	PassthroughRequests  uint64 `json:"passthrough_requests"`
	HydratedRepositories uint64 `json:"hydrated_repositories"`
	RejectedRequests     uint64 `json:"rejected_requests"`
	PackCacheHits        uint64 `json:"pack_cache_hits"`
}

type adminStatus struct {
	Status            string      `json:"status"`
	Version           string      `json:"version"`
	Mode              string      `json:"mode"`
	Listen            string      `json:"listen"`
	UptimeSeconds     float64     `json:"uptime_seconds"`
	ActiveRequests    int         `json:"active_requests"`
	ActiveSSHSessions int         `json:"active_ssh_sessions"`
	Cache             adminCache  `json:"cache"`
	DiskFreeBytes     *int64      `json:"disk_free_bytes"`
	Totals            adminTotals `json:"totals"`
}

type adminAck struct {
	Status            string `json:"status"`
	ActiveRequests    int    `json:"active_requests"`
	ActiveSSHSessions int    `json:"active_ssh_sessions"`
}

func (s *Server) adminStatus(w http.ResponseWriter, r *http.Request) {
	stats := s.store.Stats()
	// A stat error is a degraded answer, not a failed one: the disk figure
	// turns null and the rest still reports.
	var diskFree *int64
	if free, err := s.store.DiskFree(); err == nil {
		diskFree = &free
	}
	writeJSON(w, http.StatusOK, adminStatus{
		Status:            s.drainingLabel(),
		Version:           buildinfo.Version,
		Mode:              string(s.config.Mode),
		Listen:            s.config.Listen,
		UptimeSeconds:     time.Since(s.started).Seconds(),
		ActiveRequests:    s.requests.Load().active(),
		ActiveSSHSessions: s.sshSessionCount(),
		Cache:             adminCache{Entries: stats.Entries, Bytes: stats.Bytes, MaxBytes: stats.MaxBytes, ReservedBytes: stats.ReservedBytes},
		DiskFreeBytes:     diskFree,
		Totals: adminTotals{LocalRequests: s.local.Load(), PassthroughRequests: s.proxied.Load(), HydratedRepositories: s.hydrated.Load(),
			RejectedRequests: s.rejected.Load(), PackCacheHits: s.packHits.Load()},
	})
}

func (s *Server) adminScopes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		Scopes []cache.Scope `json:"scopes"`
	}{Scopes: s.store.Scopes()})
}

func (s *Server) adminDrain(w http.ResponseWriter, r *http.Request) {
	s.SetDraining(true)
	s.logger.Info("node draining", "active_requests", s.requests.Load().active(), "active_ssh_sessions", s.sshSessionCount())
	s.writeAck(w)
}

func (s *Server) adminUndrain(w http.ResponseWriter, r *http.Request) {
	s.SetDraining(false)
	s.logger.Info("node accepting requests")
	s.writeAck(w)
}

func (s *Server) writeAck(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, adminAck{Status: s.drainingLabel(), ActiveRequests: s.requests.Load().active(), ActiveSSHSessions: s.sshSessionCount()})
}

func (s *Server) drainingLabel() string {
	if s.drain.Load() {
		return "draining"
	}
	return "ok"
}

func (s *Server) adminReload(w http.ResponseWriter, r *http.Request) {
	if s.configPath == "" {
		http.Error(w, "configuration path unknown: the server did not start from a file", http.StatusInternalServerError)
		return
	}
	next, err := config.Load(s.configPath)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	applied, restart, err := s.ApplyConfig(next)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if applied == nil {
		applied = []ConfigChange{}
	}
	if restart == nil {
		restart = []ConfigChange{}
	}
	s.logger.Info("configuration reloaded", "applied", len(applied), "restart_required", len(restart))
	writeJSON(w, http.StatusOK, struct {
		Applied         []ConfigChange `json:"applied"`
		RestartRequired []ConfigChange `json:"restart_required"`
	}{applied, restart})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
