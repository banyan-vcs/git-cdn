package server

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// bigFixture returns a fixture whose tip carries an incompressible blob, so
// response packs are large enough to exercise block streaming and queue
// backpressure deterministically.
func bigFixture(t *testing.T, size int) *fixture {
	t.Helper()
	f := newFixture(t)
	blob := make([]byte, size)
	_, err := rand.Read(blob)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "big.bin"), blob, 0600))
	testGit(t, f.work, "add", "big.bin")
	testGit(t, f.work, "commit", "--quiet", "-m", "big blob")
	testGit(t, f.work, "push", "--quiet", f.repo, "main")
	f.tip = testGit(t, f.work, "rev-parse", "HEAD")
	return f
}

func bodyDigest(t *testing.T, r *http.Response) string {
	t.Helper()
	hash := sha256.New()
	_, err := io.Copy(hash, r.Body)
	require.NoError(t, err)
	require.EqualValues(t, 200, r.StatusCode)
	return hex.EncodeToString(hash.Sum(nil))
}

// Concurrent identical warm requests stream one broadcast: every response
// carries the same bytes as the generation that warmed the entry.
func TestResponseFanoutServesIdenticalBodies(t *testing.T) {
	f := bigFixture(t, 512<<10)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	reference := bodyDigest(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))

	const readers = 8
	digests := make([]string, readers)
	var wg sync.WaitGroup
	for i := range readers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			digests[i] = bodyDigest(t, r)
		}()
	}
	wg.Wait()
	for i := range readers {
		require.Equal(t, reference, digests[i], "broadcast response %d diverged", i)
	}
	require.GreaterOrEqual(t, metricValue(t, h, "gitcdn_response_fanout_subscribers_total"), 1, "warm responses did not subscribe to a broadcast")
	require.EqualValues(t, readers+1, s.local.Load(), "warm responses were not served locally")
}

// A subscriber that cannot keep up detaches from the broadcast and finishes
// by reading the stored file from its own offset; its response still
// matches byte for byte.
func TestResponseFanoutDetachesSlowReaders(t *testing.T) {
	f := bigFixture(t, 1536<<10)
	_, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	reference := bodyDigest(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil))

	slowDigest := make(chan string, 1)
	go func() {
		r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
		hash := sha256.New()
		chunk := make([]byte, 16<<10)
		for {
			n, err := r.Body.Read(chunk)
			hash.Write(chunk[:n])
			if err != nil {
				break
			}
			time.Sleep(15 * time.Millisecond)
		}
		require.EqualValues(t, 200, r.StatusCode)
		r.Body.Close()
		slowDigest <- hex.EncodeToString(hash.Sum(nil))
	}()
	// The fast reader drives the pump past the slow reader's queue.
	for range 3 {
		require.Equal(t, reference, bodyDigest(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)))
	}
	require.Equal(t, reference, <-slowDigest, "detached slow reader received divergent bytes")
	require.GreaterOrEqual(t, metricValue(t, h, "gitcdn_response_fanout_detached_total"), 1, "slow reader was never detached")
	require.GreaterOrEqual(t, metricValue(t, h, "gitcdn_response_fanout_subscribers_total"), 2, "fast readers did not join the slow reader's broadcast")
}

// Under a tight soft memory limit, a concurrent mixed workload still serves
// correctly: identical responses stay byte-equal, the batch collapses, and
// the process reports its heap below the limit.
func TestLowMemoryLimitServesConcurrentWorkload(t *testing.T) {
	f := bigFixture(t, 768<<10)
	s, h := newCDN(t, f, nil)
	previous := debug.SetMemoryLimit(32 << 20)
	defer debug.SetMemoryLimit(previous)

	full := fetchBody(f.tip)
	shallow := fetchBody(f.tip, "deepen 1")
	referenceFull := bodyDigest(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", full, testAuthorization, nil))
	referenceShallow := bodyDigest(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", shallow, testAuthorization, nil))

	const workers = 16
	var wg sync.WaitGroup
	failures := make(chan string, workers)
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var want string
			body := full
			if i%3 == 0 {
				body, want = shallow, referenceShallow
			} else {
				want = referenceFull
			}
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
			if got := bodyDigest(t, r); got != want {
				failures <- fmt.Sprintf("worker %d diverged", i)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for failure := range failures {
		t.Fatal(failure)
	}
	r, err := http.Get(h.URL + "/healthz")
	require.NoError(t, err)
	r.Body.Close()
	require.EqualValues(t, 200, r.StatusCode, "service unhealthy after the low-memory workload")

	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	t.Logf("low-memory workload: heap in use %d MiB, GC cycles %d", stats.HeapInuse>>20, stats.NumGC)
	require.Less(t, int64(stats.HeapInuse), int64(48<<20), "heap exceeded the stress envelope")
	require.GreaterOrEqual(t, metricValue(t, h, "gitcdn_response_fanout_subscribers_total"), 1, "workload never used the broadcast")
	// The full hydration is the only origin fetch: the shallow entry
	// generates locally on the warm scope.
	require.EqualValues(t, 1, f.fetches.Load(), "workload returned to the origin")
	require.EqualValues(t, 2+workers, s.local.Load(), "workload was not served locally")
}
