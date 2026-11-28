package cache

import (
	"math"
	"sort"
	"time"
)

// The overview answers how many repositories the cache holds, how idle and
// how large they are, how they split over sites and how concentrated the
// requests are, without naming any repository. It is computed on request:
// one pass over the scopes under the store lock copies what each one holds,
// and the counting is done outside it.

// activitySlots is the length of the ring of hourly request counts a scope
// keeps: the hour being written and the day before it.
const activitySlots = 25

// IdleThresholds are the lower bounds of the cumulative idle buckets, the
// shortest first: each bucket counts the scopes last served at least that
// long ago, the oldest included.
var IdleThresholds = []time.Duration{time.Hour, 6 * time.Hour, 24 * time.Hour, 3 * 24 * time.Hour, 7 * 24 * time.Hour, 14 * 24 * time.Hour, 30 * 24 * time.Hour}

// SizeThresholds are the lower bounds of the cumulative size buckets: each
// counts the scopes holding at least that many bytes.
var SizeThresholds = []int64{1 << 20, 16 << 20, 128 << 20, 1 << 30, 8 << 30}

// ActiveWindows are the windows a scope is counted as active in when it
// was served within them.
var ActiveWindows = []time.Duration{time.Hour, 24 * time.Hour, 7 * 24 * time.Hour}

// SiteTotals is what one upstream site holds in the cache.
type SiteTotals struct {
	Repositories int64
	Bytes        int64
	// Requests counts the fetches served for the site's scopes since the
	// process started.
	Requests int64
}

type Overview struct {
	// Repositories counts the scopes that hold cache bytes; every other
	// figure describes those.
	Repositories int64
	// Idle and IdleBytes are per IdleThresholds.
	Idle, IdleBytes []int64
	// Large and LargeBytes are per SizeThresholds.
	Large, LargeBytes []int64
	// Active is per ActiveWindows.
	Active []int64
	// Largest, Median and P90 are repository sizes; the quantiles are
	// nearest-rank.
	Largest, Median, P90 int64
	// PackBytes is held by response-pack entries, which are part of the
	// repository bytes.
	PackBytes int64
	// Top10Share is the share of the last day's served requests that went
	// to the ten most requested repositories; zero without requests.
	Top10Share float64
	Sites      map[string]SiteTotals
	// HydrationBackingOff and MaintenanceBackingOff count the scopes whose
	// last hydration, or last GC, repack or reconciliation, failed and is
	// still waiting to be retried.
	HydrationBackingOff   int64
	MaintenanceBackingOff int64
}

func (s *Store) Overview(now time.Time) Overview {
	type held struct {
		bytes, packs, requests int64
		idle                   time.Duration
		site                   string
	}
	scopes := make([]held, 0, len(s.entries))
	hour := now.Unix() / 3600
	o := Overview{
		Idle: make([]int64, len(IdleThresholds)), IdleBytes: make([]int64, len(IdleThresholds)),
		Large: make([]int64, len(SizeThresholds)), LargeBytes: make([]int64, len(SizeThresholds)),
		Active: make([]int64, len(ActiveWindows)),
		Sites:  make(map[string]SiteTotals),
	}
	s.mu.Lock()
	for _, e := range s.entries {
		if e.size <= 0 {
			continue
		}
		one := held{bytes: e.size, idle: now.Sub(e.accessed), site: e.site}
		for _, p := range e.packs {
			one.packs += p.size
		}
		if hour-e.activityHour < activitySlots {
			for _, count := range e.activity {
				one.requests += int64(count)
			}
		}
		if !e.gc.Due(now) || !e.repack.Due(now) || !e.reconcile.Due(now) {
			o.MaintenanceBackingOff++
		}
		if one.site == "" {
			one.site = UnknownSite
		}
		scopes = append(scopes, one)
	}
	for _, backoff := range s.hydrationBackoffs {
		if !backoff.Due(now) {
			o.HydrationBackingOff++
		}
	}
	for site, requests := range s.siteRequests {
		o.Sites[site] = SiteTotals{Requests: requests}
	}
	s.mu.Unlock()

	o.Repositories = int64(len(scopes))
	sizes := make([]int64, len(scopes))
	var requests []int64
	var served int64
	for i, one := range scopes {
		sizes[i] = one.bytes
		o.Largest = max(o.Largest, one.bytes)
		for i, threshold := range IdleThresholds {
			if one.idle < threshold {
				break
			}
			o.Idle[i]++
			o.IdleBytes[i] += one.bytes
		}
		for i, threshold := range SizeThresholds {
			if one.bytes < threshold {
				break
			}
			o.Large[i]++
			o.LargeBytes[i] += one.bytes
		}
		for i, window := range ActiveWindows {
			if one.idle < window {
				o.Active[i]++
			}
		}
		site := o.Sites[one.site]
		site.Repositories++
		site.Bytes += one.bytes
		o.Sites[one.site] = site
		o.PackBytes += one.packs
		if one.requests > 0 {
			requests = append(requests, one.requests)
			served += one.requests
		}
	}
	sort.Slice(sizes, func(i, j int) bool { return sizes[i] < sizes[j] })
	o.Median, o.P90 = rank(sizes, 0.5), rank(sizes, 0.9)
	o.Top10Share = topShare(requests, served)
	return o
}

// rank is the nearest-rank quantile of sorted values: the smallest value
// that at least the share q of the values do not exceed.
func rank(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[max(int(math.Ceil(q*float64(len(sorted)))), 1)-1]
}

// topShare is the part of served that the ten largest summands hold.
func topShare(requests []int64, served int64) float64 {
	if served == 0 {
		return 0
	}
	sort.Slice(requests, func(i, j int) bool { return requests[i] > requests[j] })
	var top int64
	for _, count := range requests[:min(10, len(requests))] {
		top += count
	}
	return float64(top) / float64(served)
}

// Served records a fetch the cache answered for the scope: it counts the
// request for the scope's recent activity and for its site, and remembers
// the site the scope belongs to — the upstream alias of a multi-site
// deployment, or the single upstream in single-site mode.
func (s *Store) Served(key, site string) {
	if !validKey(key) {
		return
	}
	now := time.Now()
	s.mu.Lock()
	e := s.entries[key]
	if e == nil {
		s.mu.Unlock()
		return
	}
	e.countActivity(now)
	if s.siteRequests == nil {
		s.siteRequests = make(map[string]int64)
	}
	s.siteRequests[site]++
	moved := e.site != site
	e.site = site
	s.mu.Unlock()
	if moved {
		s.persistSite(key, site)
	}
}

// countActivity notes a request in the scope's ring of hourly counts.
// Caller holds Store.mu.
func (e *entry) countActivity(now time.Time) {
	hour := now.Unix() / 3600
	if e.activityHour != hour {
		switch {
		case e.activity == nil:
			e.activity = make([]uint32, activitySlots)
		case hour-e.activityHour >= activitySlots:
			clear(e.activity)
		default:
			for passed := e.activityHour + 1; passed <= hour; passed++ {
				e.activity[passed%activitySlots] = 0
			}
		}
		e.activityHour = hour
	}
	e.activity[hour%activitySlots]++
}

// DiskFree reports the bytes available to this process on the cache volume.
func (s *Store) DiskFree() (int64, error) {
	return freeBytes(s.Root)
}
