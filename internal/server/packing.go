package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

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

// tagRoots expands the root set with snapshot tags whose targets descend
// from wants, bounded by the local validation budget.
func tagRoots(ctx context.Context, lease *cache.Lease, snapshot protocol.Snapshot, wants []string, base int) ([]string, error) {
	var roots []string
	tagCount := 0
	for _, ref := range snapshot.Refs {
		if !strings.HasPrefix(ref.Name, "refs/tags/") {
			continue
		}
		tagCount++
		if tagCount > 128 || ctx.Err() != nil {
			return nil, errors.New("tag validation exceeds local budget")
		}
		target := ref.OID
		if ref.Peeled != "" {
			target = ref.Peeled
		}
		if lease.Ancestor(ctx, target, wants) {
			roots = append(roots, ref.OID)
		}
		if len(roots)+base > 128 {
			return nil, errors.New("too many tag roots")
		}
	}
	return roots, nil
}

// serveShared generates a response under a shared reader lease, alongside
// other readers, while hydration and maintenance wait. The second return
// value reports whether the response was served (false means the caller
// must retry under the exclusive lease — missing objects, unsupported
// shape, or a generation failure worth retrying); the third reports a
// response-pack hit.
func (s *Server) serveShared(ctx context.Context, lease *cache.Lease, scope string, key []byte, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, bool, bool) {
	if key != nil {
		if stored, err := lease.LookupPack(key); err == nil && stored != nil {
			s.packHits.Add(1)
			// Convert the reader lease into a streaming pin: the response
			// outlives it, but eviction cannot remove its scope mid-stream.
			return &responseFile{file: stored, budget: s.responses, limit: s.config.MaxResponseBytes, persist: true, unpin: s.store.Pin(scope)}, true, true
		}
	}
	roots := unique(fetch.Wants)
	if !lease.Complete(ctx, roots) {
		return nil, false, false
	}
	for _, have := range unique(fetch.Haves) {
		// The exclusive path reports the precise failure and falls back.
		if !lease.Ancestor(ctx, have, roots) {
			return nil, false, false
		}
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
	view, err := lease.View(ctx, snapshot, roots)
	if err != nil {
		return nil, false, false
	}
	defer os.RemoveAll(view)
	output, err := os.CreateTemp(s.store.Tmp, "response-")
	if err != nil {
		return nil, false, false
	}
	file := &responseFile{file: output, budget: s.responses, limit: s.config.MaxResponseBytes}
	failed := true
	defer func() {
		if failed {
			file.Close()
		}
	}()
	if err := s.git.Run(ctx, view, bytes.NewReader(data), file, map[string]string{"GIT_PROTOCOL": "version=2"}, "upload-pack", "--stateless-rpc", view); err != nil {
		return nil, false, false
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, false, false
	}
	if key != nil {
		if stored, err := lease.StorePack(key, output.Name()); err != nil {
			s.logger.Debug("response pack cache store failed", "error", err)
		} else if stored {
			s.packStores.Add(1)
		}
	}
	failed = false
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
				return &responseFile{file: stored, budget: s.responses, limit: s.config.MaxResponseBytes, persist: true, unpin: unpin}, "HIT", true, nil
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
		if stored, err := lease.LookupPack(key); err == nil && stored != nil {
			s.packHits.Add(1)
			// Convert the lease into a streaming pin: the response outlives
			// the gate, but eviction cannot remove its scope mid-stream.
			return &responseFile{file: stored, budget: s.responses, limit: s.config.MaxResponseBytes, persist: true, unpin: s.store.Pin(scope)}, "HIT", true, nil
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
	for _, have := range unique(fetch.Haves) {
		if !lease.Ancestor(ctx, have, roots) {
			return nil, "", false, errors.New("have is outside authorized roots")
		}
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
	view, err := lease.View(ctx, snapshot, roots)
	if err != nil {
		return nil, "", false, err
	}
	defer os.RemoveAll(view)
	output, err := os.CreateTemp(s.store.Tmp, "response-")
	if err != nil {
		return nil, "", false, err
	}
	file := &responseFile{file: output, budget: s.responses, limit: s.config.MaxResponseBytes}
	failed := true
	defer func() {
		if failed {
			file.Close()
		}
	}()
	err = s.git.Run(ctx, view, bytes.NewReader(data), file, map[string]string{"GIT_PROTOCOL": "version=2"}, "upload-pack", "--stateless-rpc", view)
	if err != nil {
		return nil, "", false, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", false, err
	}
	if key != nil {
		if stored, err := lease.StorePack(key, output.Name()); err != nil {
			s.logger.Debug("response pack cache store failed", "error", err)
		} else if stored {
			s.packStores.Add(1)
		}
	}
	failed = false
	status := "MISS"
	if hit {
		status = "HIT"
	}
	return file, status, false, nil
}
