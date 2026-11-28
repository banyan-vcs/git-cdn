package cache

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The overview is counted from what the store holds in memory. Twelve
// scopes with known sizes, idle times, sites and request counts exercise
// every bucket, the quantiles and the top-10 share without naming one.
func TestOverviewCountsBucketsQuantilesAndShare(t *testing.T) {
	s, err := Open(t.TempDir(), nil, 8<<30)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	now := time.Now()
	hour := now.Unix() / 3600
	for i := 1; i <= 12; i++ {
		e := newEntry(fmt.Sprintf("%064x", i))
		e.size = int64(i) << 24
		e.accessed = now.Add(-time.Duration(i-1) * time.Hour)
		e.site = "gitlab"
		if i%2 == 0 {
			e.site = "github"
		}
		requests := 5
		switch i {
		case 1:
			requests, e.packs = 40, map[string]*packEntry{"a": {size: 1 << 10}}
		case 2:
			requests, e.packs = 10, map[string]*packEntry{"b": {size: 2 << 10}}
		case 12:
			// Requests from before the last day are outside the window.
			e.activity, e.activityHour = make([]uint32, activitySlots), hour-activitySlots
			for slot := range e.activity {
				e.activity[slot] = 100
			}
			requests = 0
		}
		for range requests {
			e.countActivity(now)
		}
		s.entries[e.key] = e
		s.usedBytes += e.size
	}
	s.hydrationFailed(fmt.Sprintf("%064x", 1))
	s.hydrationBackoffs[fmt.Sprintf("%064x", 2)] = &Backoff{}
	s.entries[fmt.Sprintf("%064x", 3)].repack.Fail(now, maintenanceFailureBackoff, failureBackoffLimit)

	o := s.Overview(now)
	require.EqualValues(t, 12, o.Repositories)
	// Idle for at least an hour: eleven, holding 77 MiB; for at least six
	// hours: six, holding 57 MiB.
	require.Equal(t, []int64{11, 6, 0, 0, 0, 0, 0}, o.Idle)
	require.Equal(t, []int64{77 << 24, 57 << 24, 0, 0, 0, 0, 0}, o.IdleBytes)
	// Sizes are 16 MiB times 1 to 12.
	require.Equal(t, []int64{12, 12, 5, 0, 0}, o.Large)
	require.Equal(t, []int64{78 << 24, 78 << 24, 50 << 24, 0, 0}, o.LargeBytes)
	require.Equal(t, []int64{1, 12, 12}, o.Active)
	require.EqualValues(t, 12<<24, o.Largest)
	require.EqualValues(t, 6<<24, o.Median)
	require.EqualValues(t, 11<<24, o.P90)
	require.EqualValues(t, 3<<10, o.PackBytes)
	// Scope 1 took 40 of the 95 requests inside the window. The top ten hold
	// every request but the five of the smallest of the eleven scopes served.
	require.InDelta(t, 90.0/95, o.Top10Share, 1e-9)
	require.Equal(t, map[string]SiteTotals{
		"gitlab": {Repositories: 6, Bytes: 36 << 24},
		"github": {Repositories: 6, Bytes: 42 << 24},
	}, o.Sites)
	require.EqualValues(t, 1, o.HydrationBackingOff, "only a failure still inside its window counts")
	require.EqualValues(t, 1, o.MaintenanceBackingOff)
}

// A scope is counted under the site it was last served for, again after a
// restart; the request counts start over with the process.
func TestOverviewRemembersSitesAcrossRestarts(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, nil, 8<<30)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	first, second := s.Key("one"), s.Key("two")
	for _, key := range []string{first, second} {
		require.NoError(t, s.ensureScope(key))
		l, err := s.Acquire(context.Background(), key)
		require.NoError(t, err)
		s.Served(key, "gitlab")
		l.Release()
	}
	s.Served(first, "gitlab")
	s.Served(second, "github")
	s.Served(second, "ssh:internal")
	require.Equal(t, "ssh:internal", string(mustRead(t, filepath.Join(root, "repos", second, siteFile))))
	s.siteRequests["forgotten"] = 9
	o := s.Overview(time.Now())
	// Bytes follow the scope's last rescan, which the site record outgrew.
	require.Equal(t, map[string]SiteTotals{
		"gitlab": {Repositories: 1, Bytes: 6, Requests: 3}, "github": {Requests: 1},
		"ssh:internal": {Repositories: 1, Bytes: 6, Requests: 1}, "forgotten": {Requests: 9},
	}, o.Sites)
	require.NoError(t, s.Close())

	s, err = Open(root, nil, 8<<30)
	require.NoError(t, err)
	o = s.Overview(time.Now())
	require.Equal(t, map[string]SiteTotals{"gitlab": {Repositories: 1, Bytes: 6}, "ssh:internal": {Repositories: 1, Bytes: 12}}, o.Sites)

	// A record that is not a site alias leaves its scope under the unknown site.
	require.NoError(t, os.WriteFile(filepath.Join(root, "repos", first, siteFile), []byte("git\nlab\n"), 0600))
	require.NoError(t, s.Close())
	s, err = Open(root, nil, 8<<30)
	require.NoError(t, err)
	o = s.Overview(time.Now())
	require.Contains(t, o.Sites, UnknownSite)
	require.EqualValues(t, 1, o.Sites[UnknownSite].Repositories)
	require.NoError(t, s.Close())
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return data
}

func TestDiskFreeMatchesTheCacheVolume(t *testing.T) {
	s, err := Open(t.TempDir(), nil, 8<<30)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, s.Close()) })
	free, err := s.DiskFree()
	require.NoError(t, err)
	require.Greater(t, free, int64(1<<20))
}
