package server

import (
	"crypto/sha256"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// drain consumes a response fully and returns its digest, measuring the
// transfer from the caller's perspective.
func drain(t *testing.T, base string, body []byte) (string, time.Duration) {
	t.Helper()
	start := time.Now()
	r := postFetch(t, base, "/gitlab/Group/Repo.git/git-upload-pack", body, testAuthorization, nil)
	hash := sha256.New()
	_, err := io.Copy(hash, r.Body)
	require.NoError(t, err)
	require.EqualValues(t, 200, r.StatusCode)
	return string(hash.Sum(nil)), time.Since(start)
}

// Concurrent warm requests must not serialize behind per-block or
// per-response global work: a healthy run serves every concurrent request
// in the ballpark of a sequential one, while a serialization regression
// stretches each request toward the sum of all of them. The bound is
// deliberately generous so slow CI runners stay inside it.
func TestWarmConcurrentLatencyDoesNotCollapse(t *testing.T) {
	f := bigFixture(t, 1<<20)
	_, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	reference, _ := drain(t, h.URL, body)
	require.NotEmpty(t, reference)

	const sequential = 10
	worst := time.Duration(0)
	for range sequential {
		digest, elapsed := drain(t, h.URL, body)
		require.Equal(t, reference, digest)
		if elapsed > worst {
			worst = elapsed
		}
	}

	const concurrent = 12
	elapsed := make([]time.Duration, concurrent)
	var wg sync.WaitGroup
	for i := range concurrent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			digest, took := drain(t, h.URL, body)
			require.Equal(t, reference, digest)
			elapsed[i] = took
		}()
	}
	wg.Wait()
	for i := range concurrent {
		require.Less(t, elapsed[i], 10*worst, "concurrent request %d serialized: %v vs worst sequential %v", i, elapsed[i], worst)
	}
}

// Warm hits on a pooled broadcast reuse block buffers instead of
// reallocating the response per request. The budget is generous — HTTP
// machinery dominates — but catches per-response buffer regressions.
func TestWarmHitAllocationBudget(t *testing.T) {
	f := bigFixture(t, 512<<10)
	_, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	for range 5 { // warm entry, fill the broadcast buffer pool
		digest, _ := drain(t, h.URL, body)
		require.NotEmpty(t, digest)
	}
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	const requests = 40
	for range requests {
		digest, _ := drain(t, h.URL, body)
		require.NotEmpty(t, digest)
	}
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	perRequest := (after.TotalAlloc - before.TotalAlloc) / requests
	t.Logf("warm hit allocations: %d bytes per %d-byte response", perRequest, 512<<10)
	require.Less(t, int64(perRequest), int64(512<<10), "warm hits allocate more than the response size per request")
}

// Broadcast buffers return to the registry free list after a stream ends
// and are reused by the next stream.
func TestFanoutBuffersAreReused(t *testing.T) {
	f := bigFixture(t, 512<<10)
	s, h := newCDN(t, f, nil)
	body := fetchBody(f.tip)
	for range 3 {
		digest, _ := drain(t, h.URL, body)
		require.NotEmpty(t, digest)
	}
	require.Greater(t, len(s.fanouts.pool), 0, "broadcast buffers were not returned to the free list")
}
