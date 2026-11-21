package server

import (
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"

	"github.com/stretchr/testify/require"
)

// TestPackCacheEffectMeasurement compares serving identical clone-shaped
// requests with the response pack cache off versus on. It is a measurement,
// not a gate: run with GIT_CDN_PACK_BENCH=1 to print the timing table.
func TestPackCacheEffectMeasurement(t *testing.T) {
	if os.Getenv("GIT_CDN_PACK_BENCH") == "" {
		t.Skip("set GIT_CDN_PACK_BENCH=1 to measure the response pack cache")
	}
	f := newFixture(t)
	random := rand.New(rand.NewSource(42))
	blob := make([]byte, 64<<10)
	for i := range 80 {
		random.Read(blob)
		require.NoError(t, os.WriteFile(filepath.Join(f.work, "data.bin"), blob, 0600))
		f.commit(t, "bulk "+string(rune('a'+i%26)))
	}
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	body := fetchBody(f.tip)

	measure := func(label string, packCache int64) []time.Duration {
		_, h := newCDN(t, f, func(c *config.Config) {
			c.MaxPackCacheBytes = packCache
			c.CacheDir = t.TempDir() // cold start per run
		})
		var durations []time.Duration
		for range 5 {
			start := time.Now()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			responseBody(t, r)
			durations = append(durations, time.Since(start))
		}
		for i, d := range durations {
			t.Logf("%s request %d: %v", label, i+1, d)
		}
		return durations
	}
	off := measure("pack-cache off", -1)
	on := measure("pack-cache on", 0)
	warmOff := off[len(off)-1]
	warmOn := on[len(on)-1]
	t.Logf("pack-cache off warm serve: %v; pack-cache on warm serve: %v; ratio %.2fx", warmOff, warmOn, float64(warmOff)/float64(warmOn))
}

// TestPackCacheConcurrencyMeasurement compares parallel warm serving with
// the response pack cache off versus on. Without the cache, same-scope
// requests serialize on the repository gate and each runs upload-pack;
// with it, hits pin the scope and stream in parallel. It also reports the
// parallel warm incremental-fetch time — the shared reader path that never
// enters the pack cache. It is a measurement, not a gate: run with
// GIT_CDN_PACK_BENCH=1.
func TestPackCacheConcurrencyMeasurement(t *testing.T) {
	if os.Getenv("GIT_CDN_PACK_BENCH") == "" {
		t.Skip("set GIT_CDN_PACK_BENCH=1 to measure the response pack cache")
	}
	f := newFixture(t)
	random := rand.New(rand.NewSource(42))
	blob := make([]byte, 64<<10)
	for i := range 80 {
		random.Read(blob)
		require.NoError(t, os.WriteFile(filepath.Join(f.work, "data.bin"), blob, 0600))
		f.commit(t, "bulk "+string(rune('a'+i%26)))
	}
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	body := fetchBody(f.tip)
	incremental := fetchBody(f.tip, "have "+f.tip)
	const workers, perWorker = 8, 10
	measure := func(label string, packCache int64, fetchBody []byte) time.Duration {
		_, h := newCDN(t, f, func(c *config.Config) {
			c.MaxPackCacheBytes = packCache
			c.CacheDir = t.TempDir() // cold start per run
		})
		responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
		start := time.Now()
		var wg sync.WaitGroup
		for range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perWorker {
					responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody, testAuthorization, nil))
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		t.Logf("%s: %d parallel warm requests in %v", label, workers*perWorker, elapsed)
		return elapsed
	}
	off := measure("pack-cache off", -1, body)
	on := measure("pack-cache on", 0, body)
	t.Logf("parallel warm clone ratio %.2fx", float64(off)/float64(on))
	measure("incremental fetches (shared readers)", 0, incremental)
}

// TestShallowBatchMeasurement reports the depth=1 fleet scenario: many
// concurrent identical shallow fetches of the same tip. With shallow
// eligibility each batch costs one generation plus broadcasts; with the pack
// cache off every request runs its own upload-pack. It is a measurement,
// not a gate: run with GIT_CDN_PACK_BENCH=1.
func TestShallowBatchMeasurement(t *testing.T) {
	if os.Getenv("GIT_CDN_PACK_BENCH") == "" {
		t.Skip("set GIT_CDN_PACK_BENCH=1 to measure the shallow batch path")
	}
	f := newFixture(t)
	random := rand.New(rand.NewSource(42))
	blob := make([]byte, 64<<10)
	for i := range 80 {
		random.Read(blob)
		require.NoError(t, os.WriteFile(filepath.Join(f.work, "data.bin"), blob, 0600))
		f.commit(t, "bulk "+string(rune('a'+i%26)))
	}
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	shallow := fetchBody(f.tip, "deepen 1")
	const clients = 16
	measure := func(label string, packCache int64) time.Duration {
		_, h := newCDN(t, f, func(c *config.Config) {
			c.MaxPackCacheBytes = packCache
			c.CacheDir = t.TempDir() // cold start per run
		})
		// Warm the scope with a full clone once so the batch measures the
		// shallow serving path, not first hydration.
		responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
		start := time.Now()
		var wg sync.WaitGroup
		for range clients {
			wg.Add(1)
			go func() {
				defer wg.Done()
				responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", shallow, testAuthorization, nil))
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		t.Logf("%s: %d concurrent shallow fetches in %v", label, clients, elapsed)
		return elapsed
	}
	off := measure("shallow batch pack-cache off", -1)
	on := measure("shallow batch pack-cache on", 0)
	t.Logf("shallow batch ratio %.2fx", float64(off)/float64(on))
}

// BenchmarkWarmHitServe measures the steady-state warm hit: pin, indexed
// lookup, broadcast stream and unpin, with no generation. Run with
// go test -bench BenchmarkWarmHitServe -benchmem.
func BenchmarkWarmHitServe(b *testing.B) {
	t := &testing.T{}
	f := newFixture(t)
	random := rand.New(rand.NewSource(42))
	blob := make([]byte, 64<<10)
	for i := range 40 {
		random.Read(blob)
		require.NoError(b, os.WriteFile(filepath.Join(f.work, "data.bin"), blob, 0600))
		f.commit(t, "bulk "+string(rune('a'+i%26)))
	}
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	_, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	b.ResetTimer()
	for range b.N {
		responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
	}
}

// TestWarmLatencyByConcurrency reports per-request latency as concurrency
// rises on one warm entry: the spread exposes serialization on the hot
// path. It is a measurement, not a gate: run with GIT_CDN_PACK_BENCH=1.
func TestWarmLatencyByConcurrency(t *testing.T) {
	if os.Getenv("GIT_CDN_PACK_BENCH") == "" {
		t.Skip("set GIT_CDN_PACK_BENCH=1 to measure warm latency curves")
	}
	f := bigFixture(t, 1<<20)
	_, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	reference, _ := drain(t, h.URL, body)
	for _, workers := range []int{1, 4, 16, 64} {
		elapsed := make([]time.Duration, workers)
		var wg sync.WaitGroup
		for i := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				digest, took := drain(t, h.URL, body)
				require.Equal(t, reference, digest)
				elapsed[i] = took
			}()
		}
		wg.Wait()
		var total, worst time.Duration
		for _, d := range elapsed {
			total += d
			if d > worst {
				worst = d
			}
		}
		t.Logf("concurrency %2d: mean %v worst %v", workers, total/time.Duration(workers), worst)
	}
}
