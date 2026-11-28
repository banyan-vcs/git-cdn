package server

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
)

// writeMetrics renders /metrics in the Prometheus text format. Every sample
// is an integer except the one ratio; nothing carries a repository name.
func (s *Server) writeMetrics(w io.Writer) {
	var out strings.Builder
	// The counters are uint64, the cache stats a mix of int64 and int.
	gauge := func(name string, value any) {
		fmt.Fprintf(&out, "%s %d\n", name, value)
	}
	labeled := func(name, label, value string, n int64) {
		fmt.Fprintf(&out, "%s{%s=%q} %d\n", name, label, value, n)
	}
	stats := s.store.Stats()
	gauge("gitcdn_local_requests_total", s.local.Load())
	gauge("gitcdn_hydrations_total", s.hydrated.Load())
	gauge("gitcdn_passthrough_requests_total", s.proxied.Load())
	gauge("gitcdn_auth_denied_total", s.authDenied.Load())
	gauge("gitcdn_cache_bytes", stats.Bytes)
	gauge("gitcdn_cache_max_bytes", stats.MaxBytes)
	gauge("gitcdn_cache_reserved_bytes", stats.ReservedBytes)
	gauge("gitcdn_cache_entries", stats.Entries)
	gauge("gitcdn_cache_evictions_total", stats.Evictions)
	gauge("gitcdn_cache_evicted_bytes_total", stats.EvictedBytes)
	gauge("gitcdn_repacks_total", stats.Repacks)
	gauge("gitcdn_repack_failures_total", stats.RepackFailed)
	gauge("gitcdn_repack_skipped_total", stats.RepackSkipped)
	gauge("gitcdn_gcs_total", stats.GCs)
	gauge("gitcdn_gc_failures_total", stats.GCFailed)
	gauge("gitcdn_gc_skipped_total", stats.GCSkipped)
	gauge("gitcdn_reconciles_total", stats.Reconciles)
	gauge("gitcdn_roots_retired_total", stats.RootsRetired)
	gauge("gitcdn_active_requests", s.requests.active())
	gauge("gitcdn_rejected_requests_total", s.rejected.Load())
	gauge("gitcdn_temp_response_bytes", s.responses.bytes())
	gauge("gitcdn_temp_response_max_bytes", s.config.MaxTempResponseBytes)
	gauge("gitcdn_pack_cache_hits_total", s.packHits.Load())
	gauge("gitcdn_pack_cache_stores_total", s.packStores.Load())
	gauge("gitcdn_response_fanout_subscribers_total", s.fanoutSubs.Load())
	gauge("gitcdn_response_fanout_detached_total", s.fanoutDetached.Load())
	gauge("gitcdn_generation_flight_waits_total", s.flightWaits.Load())
	gauge("gitcdn_auth_probe_cache_hits_total", s.probeHits.Load())
	gauge("gitcdn_auth_probes_merged_total", s.probeMerged.Load())

	gauge("gitcdn_process_start_time_seconds", s.started.Unix())
	overview := s.store.Overview(time.Now())
	gauge("gitcdn_pack_cache_bytes", overview.PackBytes)
	if free, err := s.store.DiskFree(); err == nil {
		gauge("gitcdn_disk_free_bytes", free)
	}
	gauge("gitcdn_repositories", overview.Repositories)
	gauge("gitcdn_repository_bytes_largest", overview.Largest)
	labeled("gitcdn_repository_bytes_quantile", "quantile", "0.5", overview.Median)
	labeled("gitcdn_repository_bytes_quantile", "quantile", "0.9", overview.P90)
	for i, threshold := range cache.IdleThresholds {
		seconds := strconv.FormatInt(int64(threshold/time.Second), 10)
		labeled("gitcdn_repository_idle_bucket", "min_idle_seconds", seconds, overview.Idle[i])
		labeled("gitcdn_repository_idle_bucket_bytes", "min_idle_seconds", seconds, overview.IdleBytes[i])
	}
	for i, threshold := range cache.SizeThresholds {
		bytes := strconv.FormatInt(threshold, 10)
		labeled("gitcdn_repository_size_bucket", "min_bytes", bytes, overview.Large[i])
		labeled("gitcdn_repository_size_bucket_bytes", "min_bytes", bytes, overview.LargeBytes[i])
	}
	for i, window := range cache.ActiveWindows {
		labeled("gitcdn_active_repositories", "window", windowLabel(window), overview.Active[i])
	}
	for _, site := range sortedSites(overview.Sites) {
		totals := overview.Sites[site]
		labeled("gitcdn_site_repositories", "site", site, totals.Repositories)
		labeled("gitcdn_site_bytes", "site", site, totals.Bytes)
		labeled("gitcdn_site_requests_total", "site", site, totals.Requests)
	}
	fmt.Fprintf(&out, "gitcdn_request_top10_share %s\n", strconv.FormatFloat(overview.Top10Share, 'g', -1, 64))
	gauge("gitcdn_hydration_backoff_repositories", overview.HydrationBackingOff)
	gauge("gitcdn_maintenance_backoff_repositories", overview.MaintenanceBackingOff)
	io.WriteString(w, out.String())
}

// windowLabel names a whole number of hours the way the dashboard legends
// show it: a day stays 24h, and whole days beyond it are days.
func windowLabel(window time.Duration) string {
	hours := int64(window / time.Hour)
	if hours > 24 && hours%24 == 0 {
		return strconv.FormatInt(hours/24, 10) + "d"
	}
	return strconv.FormatInt(hours, 10) + "h"
}

func sortedSites(sites map[string]cache.SiteTotals) []string {
	names := make([]string, 0, len(sites))
	for name := range sites {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
