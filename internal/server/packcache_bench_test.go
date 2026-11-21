package server

import (
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/config"

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
// with it, hits pin the scope and stream in parallel. It is a measurement,
// not a gate: run with GIT_CDN_PACK_BENCH=1.
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
	const workers, perWorker = 8, 10
	measure := func(label string, packCache int64) time.Duration {
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
					responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		t.Logf("%s: %d parallel warm requests in %v", label, workers*perWorker, elapsed)
		return elapsed
	}
	off := measure("pack-cache off", -1)
	on := measure("pack-cache on", 0)
	t.Logf("parallel warm ratio %.2fx", float64(off)/float64(on))
}
