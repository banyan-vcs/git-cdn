package server

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Warm incremental fetches carry client haves, never enter the response
// pack cache, and therefore exercise the shared reader path.
func TestWarmIncrementalFetchesServeUnderSharedLeases(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	require.EqualValues(t, 1, f.fetches.Load())
	const requests = 6
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip, "have "+f.tip), testAuthorization, nil)
			require.NotEmpty(t, responseBody(t, r))
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, f.fetches.Load(), "warm incremental fetches rehydrated")
	require.EqualValues(t, 1, s.hydrated.Load(), "warm incremental fetches rehydrated")
	require.EqualValues(t, requests+1, s.local.Load())
}

// A snapshot change that needs no hydration serves under a shared lease;
// storage refs catch up on the background reconciler instead of the
// response path.
func TestSharedFullFetchReconcilesRefsInBackground(t *testing.T) {
	f := newFixture(t)
	s, h := newCDN(t, f, nil)
	responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	require.EqualValues(t, 1, s.hydrated.Load())
	// A tag pointing at an already-cached commit changes the snapshot
	// without requiring new objects: the shared path must still regenerate
	// (the pack key follows the snapshot) instead of reusing the old entry.
	testGit(t, f.repo, "tag", "v2", f.tip)
	r := postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil)
	require.NotEmpty(t, responseBody(t, r))
	require.EqualValues(t, "MISS", r.Header.Get("X-GitCDN-Pack-Cache"), "shared serving reused a stale-snapshot entry")
	require.EqualValues(t, 1, s.hydrated.Load(), "object-complete fetch hydrated anyway")
	identity := f.origin.URL + "/root/Group/Repo.git:sha1"
	dir := filepath.Join(s.store.Root, "repos", s.store.Key(identity), "repo.git")
	// Probe without taking the gate: a queued exclusive acquire would
	// preempt the very reconciliation this test waits for.
	require.Eventually(t, func() bool {
		_, err := s.git.Output(context.Background(), dir, "", "show-ref", "--verify", "refs/cache/upstream/tags/v2")
		return err == nil
	}, 15*time.Second, 100*time.Millisecond, "background reconciliation never recorded the new tag")
}
