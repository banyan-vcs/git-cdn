package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"github.com/chiyutianyi/git-cdn/internal/cache"
	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/chiyutianyi/git-cdn/internal/protocol"
)

func unique(values []string) []string {
	seen := make(map[string]bool)
	var result []string
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	return result
}

// scopeIdentity derives the cache scope identity from an upstream base URL
// and the client-visible repository path.
func scopeIdentity(base *url.URL, repo string) string {
	return base.String() + "/" + strings.TrimSuffix(repo, ".git") + ".git:sha1"
}

// SharedScopeIdentity reports the cache scope identity the HTTP frontend uses
// for a client-visible repository path, so a co-deployed SSH frontend can
// share the same scope. It applies only in single-upstream mode; multi-site
// deployments pin identities explicitly per upstream.
func (s *Server) SharedScopeIdentity(repo string) (string, bool) {
	if s.config.Mode != config.ModeSingle {
		return "", false
	}
	base := s.upstreams[config.SingleUpstreamID]
	if base == nil {
		return "", false
	}
	return scopeIdentity(base, repo), true
}

func (s *Server) prepare(r *http.Request, rt route, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, string, bool, error) {
	headers := s.headers(r)
	return s.prepareObjects(r.Context(), scopeIdentity(rt.base, rt.repo), fetch, snapshot, data, func(ctx context.Context, l *cache.Lease, roots []string) (bool, error) {
		return l.Ensure(ctx, rt.target(rt.repo).String(), headers, roots)
	})
}

func (s *Server) PrepareSSH(ctx context.Context, identity string, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte, source cache.HydrateSource) (*responseFile, string, bool, error) {
	file, status, packHit, err := s.prepareObjects(ctx, identity, fetch, snapshot, data, func(ctx context.Context, l *cache.Lease, roots []string) (bool, error) {
		return l.EnsureWith(ctx, roots, source)
	})
	if err == nil {
		s.local.Add(1)
	}
	return file, status, packHit, err
}

// warmResponse wraps a stored pack entry for streaming. Concurrent
// identical requests share one read through a response broadcast; a
// subscriber that cannot keep up detaches and finishes from its own file
// reads, and joining falls back to a private stream when no broadcast can
// take the subscription.
func (s *Server) warmResponse(scope string, key []byte, stored *os.File, unpin func()) *responseFile {
	fallback := &responseFile{file: stored, budget: s.responses, limit: s.config.MaxResponseBytes, persist: true, unpin: unpin}
	info, err := stored.Stat()
	if err != nil {
		return fallback
	}
	registryKey := scope + "/" + hex.EncodeToString(key)
	sub := s.fanouts.join(registryKey, stored.Name(), stored, func() { s.fanoutDetached.Add(1) })
	if sub == nil {
		return fallback
	}
	s.fanoutSubs.Add(1)
	return &responseFile{stream: sub, size: info.Size(), budget: s.responses, limit: s.config.MaxResponseBytes, persist: true, unpin: unpin}
}

// flightCache single-flights identical cacheable generations among shared
// readers of a warm scope: readers never block each other, so without this
// gate a batch of identical requests would each run upload-pack in
// parallel. The leader publishes the stored entry; waiters stream it.
type flightCache struct {
	mu sync.Mutex
	m  map[string]chan struct{}
}

// waitOrLead returns the in-flight generation's done channel for waiters,
// or a lead function the leader must call exactly once when its generation
// ends, whether it stored an entry or failed.
func (c *flightCache) waitOrLead(key string) (<-chan struct{}, func()) {
	c.mu.Lock()
	if c.m == nil {
		c.m = make(map[string]chan struct{})
	}
	if done, ok := c.m[key]; ok {
		c.mu.Unlock()
		return done, nil
	}
	done := make(chan struct{})
	c.m[key] = done
	c.mu.Unlock()
	return done, func() {
		close(done)
		c.mu.Lock()
		if c.m[key] == done {
			delete(c.m, key)
		}
		c.mu.Unlock()
	}
}

// tagRoots expands the root set with snapshot tags whose targets descend
// from wants, bounded by the local validation budget. One batched rev-list
// covers the common case where every tag target sits inside the fetched
// history; a partial miss falls back to deciding the tags individually.
func tagRoots(ctx context.Context, lease *cache.Lease, snapshot protocol.Snapshot, wants []string, base int) ([]string, error) {
	type tagRef struct {
		oid    string
		target string
	}
	var tags []tagRef
	for _, ref := range snapshot.Refs {
		if !strings.HasPrefix(ref.Name, "refs/tags/") {
			continue
		}
		if len(tags) >= 128 || ctx.Err() != nil {
			return nil, errors.New("tag validation exceeds local budget")
		}
		target := ref.OID
		if ref.Peeled != "" {
			target = ref.Peeled
		}
		tags = append(tags, tagRef{oid: ref.OID, target: target})
	}
	if len(tags) == 0 {
		return nil, nil
	}
	targets := make([]string, len(tags))
	for i, tag := range tags {
		targets[i] = tag.target
	}
	var roots []string
	if lease.AncestorsAll(ctx, targets, wants) {
		for _, tag := range tags {
			if len(roots)+base+1 > 128 {
				return nil, errors.New("too many tag roots")
			}
			roots = append(roots, tag.oid)
		}
		return roots, nil
	}
	for _, tag := range tags {
		if ctx.Err() != nil {
			return nil, errors.New("tag validation exceeds local budget")
		}
		if !lease.AncestorsAll(ctx, []string{tag.target}, wants) {
			continue
		}
		if len(roots)+base+1 > 128 {
			return nil, errors.New("too many tag roots")
		}
		roots = append(roots, tag.oid)
	}
	return roots, nil
}

// storedUnderLease serves a stored response entry while a repository lease
// is still held: the transfer outlives the gate, so the lease converts to
// a streaming pin that keeps eviction away until the transfer ends.
func (s *Server) storedUnderLease(scope string, key []byte, lease *cache.Lease) *responseFile {
	if stored, err := lease.LookupPack(key); err == nil && stored != nil {
		s.packHits.Add(1)
		return s.warmResponse(scope, key, stored, s.store.Pin(scope))
	}
	return nil
}

// generateLocal builds the request's ref view, runs upload-pack into a
// bounded spool file and publishes the response-pack entry for cacheable
// requests. It is the shared tail of the shared-lease and exclusive paths.
func (s *Server) generateLocal(ctx context.Context, lease *cache.Lease, snapshot protocol.Snapshot, roots []string, data, key []byte) (*responseFile, error) {
	view, err := lease.View(ctx, snapshot, roots)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(view)
	output, err := os.CreateTemp(s.store.Tmp, "response-")
	if err != nil {
		return nil, err
	}
	file := &responseFile{file: output, budget: s.responses, limit: s.config.MaxResponseBytes}
	failed := true
	defer func() {
		if failed {
			file.Close()
		}
	}()
	if err := s.git.Run(ctx, view, bytes.NewReader(data), file, map[string]string{"GIT_PROTOCOL": "version=2"}, "-c", "uploadpack.allowFilter=true", "upload-pack", "--stateless-rpc", view); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if key != nil {
		if stored, err := lease.StorePack(key, output.Name()); err != nil {
			s.logger.Debug("response pack cache store failed", "error", err)
		} else if stored {
			s.packStores.Add(1)
		}
	}
	failed = false
	return file, nil
}

// serveShared generates a response under a shared reader lease, alongside
// other readers, while hydration and maintenance wait. The second return
// value reports whether the response was served (false means the caller
// must retry under the exclusive lease — missing objects, unsupported
// shape, or a generation failure worth retrying); the third reports a
// response-pack hit.
func (s *Server) serveShared(ctx context.Context, lease *cache.Lease, scope string, key []byte, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, bool, bool) {
	if key != nil {
		if file := s.storedUnderLease(scope, key, lease); file != nil {
			return file, true, true
		}
	}
	roots := unique(fetch.Wants)
	if !lease.Complete(ctx, roots) {
		return nil, false, false
	}
	// The exclusive path reports the precise failure and falls back.
	if !lease.AncestorsAll(ctx, unique(fetch.Haves), roots) {
		return nil, false, false
	}
	if fetch.IncludeTag {
		tags, err := tagRoots(ctx, lease, snapshot, fetch.Wants, len(roots))
		if err != nil {
			return nil, false, false
		}
		roots = unique(append(roots, tags...))
		if !lease.Complete(ctx, roots) {
			return nil, false, false
		}
	}
	if key != nil {
		// Shared readers never block each other, so identical cacheable
		// requests on a warm scope would each run upload-pack. One reader
		// leads the generation; the rest wait and stream its entry.
		done, lead := s.flights.waitOrLead(scope + "/" + hex.EncodeToString(key))
		if lead == nil {
			s.flightWaits.Add(1)
			select {
			case <-done:
			case <-ctx.Done():
				return nil, false, false
			}
			if file := s.storedUnderLease(scope, key, lease); file != nil {
				return file, true, true
			}
			// The leader failed; this reader generates without a new flight.
		} else {
			// A leader may have published between the entry lookup above
			// and this registration.
			if file := s.storedUnderLease(scope, key, lease); file != nil {
				lead()
				return file, true, true
			}
			defer lead()
		}
	}
	file, err := s.generateLocal(ctx, lease, snapshot, roots, data, key)
	if err != nil {
		return nil, false, false
	}
	return file, true, false
}

func (s *Server) prepareObjects(ctx context.Context, identity string, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte, ensure func(context.Context, *cache.Lease, []string) (bool, error)) (*responseFile, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.gitTimeout)
	defer cancel()
	scope := s.store.Key(identity)
	key := s.packCacheKey(identity, fetch, data, snapshot)
	if key != nil {
		// Warm responses skip the repository gate entirely: pin the scope
		// against eviction, stream the stored file and never start a Git
		// process. A missing scope or entry falls through to the gated
		// path, which also collapses concurrent cold requests into a
		// single generation.
		if unpin := s.store.Pin(scope); unpin != nil {
			if stored, err := s.store.LookupPack(scope, key); err == nil && stored != nil {
				s.packHits.Add(1)
				return s.warmResponse(scope, key, stored, unpin), "HIT", true, nil
			}
			unpin()
		}
	}
	// Shared read path: when every object the request needs is already
	// cached, generation runs under a reader lease in parallel with other
	// readers of the same scope. Storage refs catch up on a background
	// writer instead of blocking the response.
	if lease, err := s.store.AcquireShared(ctx, scope); err == nil {
		if file, served, packHit := s.serveShared(ctx, lease, scope, key, fetch, snapshot, data); served {
			lease.Release()
			if fetch.FullFetch() {
				s.store.Reconcile(scope, snapshot)
			}
			return file, "HIT", packHit, nil
		}
		lease.Release()
	}
	lease, err := s.store.Acquire(ctx, scope)
	if err != nil {
		return nil, "", false, err
	}
	defer lease.Release()
	if key != nil {
		// Convert a stored hit's lease into a streaming pin: the response
		// outlives the gate, but eviction cannot remove its scope mid-stream.
		if file := s.storedUnderLease(scope, key, lease); file != nil {
			return file, "HIT", true, nil
		}
	}
	roots := unique(fetch.Wants)
	fetchCtx, fetchCancel := context.WithTimeout(ctx, s.fetchTimeout)
	hit, err := ensure(fetchCtx, lease, roots)
	fetchCancel()
	if err != nil {
		return nil, "", false, err
	}
	if !hit {
		s.hydrated.Add(1)
	}
	if !lease.AncestorsAll(ctx, unique(fetch.Haves), roots) {
		return nil, "", false, errors.New("have is outside authorized roots")
	}
	if fetch.FullFetch() {
		if err := lease.SyncRefs(ctx, snapshot); err != nil {
			return nil, "", false, err
		}
	}
	if fetch.IncludeTag {
		tags, err := tagRoots(ctx, lease, snapshot, fetch.Wants, len(roots))
		if err != nil {
			return nil, "", false, err
		}
		roots = unique(append(roots, tags...))
		fetchCtx, fetchCancel := context.WithTimeout(ctx, s.fetchTimeout)
		tagHit, err := ensure(fetchCtx, lease, roots)
		fetchCancel()
		if err != nil {
			return nil, "", false, err
		}
		if !tagHit {
			s.hydrated.Add(1)
			hit = false
		}
		if fetch.FullFetch() && !tagHit {
			if err := lease.SyncRefs(ctx, snapshot); err != nil {
				return nil, "", false, err
			}
		}
	}
	file, err := s.generateLocal(ctx, lease, snapshot, roots, data, key)
	if err != nil {
		return nil, "", false, err
	}
	status := "MISS"
	if hit {
		status = "HIT"
	}
	return file, status, false, nil
}
