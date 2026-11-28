package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
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

// prepare answers a fetch from the cache. site is the upstream alias the
// request was routed to, which the repository overview splits by.
func (s *Server) prepare(r *http.Request, rt route, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, string, bool, error) {
	headers := s.headers(r)
	return s.prepareObjects(r.Context(), scopeIdentity(rt.base, rt.repo), rt.id, fetch, snapshot, data, func(ctx context.Context, l *cache.Lease, roots []string) (bool, error) {
		return l.Ensure(ctx, rt.target(rt.repo).String(), headers, roots)
	})
}

func (s *Server) PrepareSSH(ctx context.Context, identity, site string, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte, source cache.HydrateSource) (*responseFile, string, bool, error) {
	file, status, packHit, err := s.prepareObjects(ctx, identity, site, fetch, snapshot, data, func(ctx context.Context, l *cache.Lease, roots []string) (bool, error) {
		return l.EnsureWith(ctx, roots, source)
	})
	if err == nil {
		s.local.Add(1)
	}
	return file, status, packHit, err
}

// served records a fetch the cache is answering for the scope: the request
// for the repository overview, and the reconciliation its snapshot arms.
func (s *Server) served(scope, site string, snapshot protocol.Snapshot) {
	s.store.Served(scope, site)
	s.store.Reconcile(scope, snapshot)
}

// warmResponse wraps a stored pack entry for streaming. Concurrent
// identical requests share one read through a response broadcast; a
// subscriber that cannot keep up detaches and finishes from its own file
// reads, and joining falls back to a private stream when no broadcast can
// take the subscription.
func (s *Server) warmResponse(ctx context.Context, scope string, key []byte, stored *os.File, unpin func()) *responseFile {
	fallback := &responseFile{file: stored, budget: s.responses, limit: s.config.MaxResponseBytes, persist: true, unpin: unpin}
	info, err := stored.Stat()
	if err != nil {
		return fallback
	}
	fallback.size = info.Size()
	registryKey := scope + "/" + hex.EncodeToString(key)
	sub := s.fanouts.join(registryKey, stored.Name(), stored, func() {
		s.fanoutDetached.Add(1)
		s.logger.Debug("response broadcast backpressure", "cache_key", scope, "reason", "subscriber_queue_full")
	})
	if sub == nil {
		logging.Logger(ctx, s.logger).DebugContext(ctx, "response pack stream selected", "cache_key", scope, "stream_mode", "file", "response_bytes", info.Size())
		return fallback
	}
	s.fanoutSubs.Add(1)
	logging.Logger(ctx, s.logger).DebugContext(ctx, "response pack stream selected", "cache_key", scope, "stream_mode", "broadcast", "response_bytes", info.Size())
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

// tagRoots expands the root set with the snapshot tags whose targets descend
// from wants — the tags an include-tag response can carry, because
// upload-pack sends a tag only when its target is in the pack. One batched
// decision covers every tag in the snapshot, so the count follows the
// repository and not a local budget: a repository with thousands of tags
// keeps fetching from the cache.
func tagRoots(ctx context.Context, lease *cache.Lease, snapshot protocol.Snapshot, wants []string) ([]string, error) {
	var tags []protocol.Ref
	var targets []string
	for _, ref := range snapshot.Refs {
		if !strings.HasPrefix(ref.Name, "refs/tags/") {
			continue
		}
		tags = append(tags, ref)
		targets = append(targets, tagTarget(ref))
	}
	if len(tags) == 0 {
		return nil, nil
	}
	within, err := lease.AncestorsWithin(ctx, targets, wants)
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, ref := range tags {
		if within[tagTarget(ref)] {
			roots = append(roots, ref.OID)
		}
	}
	return roots, nil
}

func tagTarget(ref protocol.Ref) string {
	if ref.Peeled != "" {
		return ref.Peeled
	}
	return ref.OID
}

// localRequest keeps only the haves that name cached commits inside the
// authorized root histories and removes the others from the request bytes.
// A developer's unpushed commits are sent as haves before anything the
// cache knows; they are not acknowledged by upload-pack either way, so the
// response to the narrowed request is a superset of the original one and
// stays correct. Upload-pack never sees a have the caller is not known to
// hold, which keeps acknowledgement from revealing an object's existence.
func localRequest(ctx context.Context, lease *cache.Lease, fetch protocol.Fetch, roots []string, data []byte) ([]byte, error) {
	if len(fetch.Haves) == 0 {
		return data, nil
	}
	within, err := lease.AncestorsWithin(ctx, fetch.Haves, roots)
	if err != nil {
		return nil, err
	}
	return protocol.FilterRequest(data, func(line string) bool {
		oid, ok := strings.CutPrefix(line, "have ")
		return ok && !within[oid]
	}), nil
}

// errGenerationBackoff reports that the response for a cache key is still
// backing off from a failed generation.
var errGenerationBackoff = errors.New("local response generation is backing off after a failure")

const (
	// generationFailureBackoff starts the wait before a failed generation is
	// attempted again; it doubles per consecutive failure up to
	// generationFailureLimit. A response that cannot be produced — larger
	// than max_response_bytes, or slower than git_timeout — would otherwise
	// be attempted for every request that asks for it.
	generationFailureBackoff = 30 * time.Second
	generationFailureLimit   = time.Hour
	generationFailureEntries = 1024
)

// generationFailures remembers, per scope and response key, the responses
// that could not be generated. The key binds the live snapshot, so a new
// tip or tag starts clean without an explicit expiry.
type generationFailures struct {
	mu      sync.Mutex
	entries map[string]*cache.Backoff
}

func generationKey(scope string, key []byte) string {
	return scope + "/" + hex.EncodeToString(key)
}

func (g *generationFailures) blocked(key string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	backoff := g.entries[key]
	return backoff != nil && !backoff.Due(time.Now())
}

func (g *generationFailures) fail(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.entries == nil {
		g.entries = make(map[string]*cache.Backoff)
	}
	backoff := g.entries[key]
	if backoff == nil {
		if len(g.entries) >= generationFailureEntries {
			// Drop expired records first; only a storm of distinct failing
			// keys evicts live ones, and a dropped record costs one retry.
			now := time.Now()
			for k, entry := range g.entries {
				if entry.Due(now) {
					delete(g.entries, k)
				}
			}
			for k := range g.entries {
				if len(g.entries) < generationFailureEntries {
					break
				}
				delete(g.entries, k)
			}
		}
		backoff = &cache.Backoff{}
		g.entries[key] = backoff
	}
	backoff.Fail(time.Now(), generationFailureBackoff, generationFailureLimit)
}

func (g *generationFailures) done(key string) {
	g.mu.Lock()
	delete(g.entries, key)
	g.mu.Unlock()
}

// recordGenerationFailure backs off a generation that failed on the
// authoritative path. Neither a canceled caller nor an exhausted spool
// budget says the response cannot be produced: the first is the client's
// doing, the second frees with other transfers.
func (s *Server) recordGenerationFailure(failure string, err error) {
	var limit responseLimitError
	if errors.Is(err, context.Canceled) || errors.As(err, &limit) && limit.reason == "temporary_response_capacity" {
		return
	}
	s.generations.fail(failure)
}

// unrecoverableGeneration reports whether a failure on the shared path
// hydration cannot repair, and therefore must not be retried under the
// exclusive lease. A response above max_response_bytes is deterministic
// and is remembered for its key; an exhausted spool budget frees with other
// transfers and is only left un-retried.
func unrecoverableGeneration(err error) bool {
	var limit responseLimitError
	return errors.As(err, &limit)
}

// storedUnderLease serves a stored response entry while a repository lease
// is still held: the transfer outlives the gate, so the lease converts to
// a streaming pin that keeps eviction away until the transfer ends.
func (s *Server) storedUnderLease(ctx context.Context, scope string, key []byte, lease *cache.Lease) *responseFile {
	if stored, err := lease.LookupPack(key); err == nil && stored != nil {
		s.packHits.Add(1)
		return s.warmResponse(ctx, scope, key, stored, s.store.Pin(scope))
	}
	return nil
}

// generateLocal builds the request's ref view, runs upload-pack into a
// bounded spool file and publishes the response-pack entry for cacheable
// requests. It is the shared tail of the shared-lease and exclusive paths.
func (s *Server) generateLocal(ctx context.Context, lease *cache.Lease, scope string, snapshot protocol.Snapshot, roots []string, data, key []byte) (file *responseFile, err error) {
	started := time.Now()
	phase := "view"
	packStore := "DISABLED"
	defer func() {
		if err != nil {
			logging.Failure(ctx, s.logger, "local pack generation failed", err,
				slog.String("cache_key", scope), slog.String("phase", phase), slog.Int64("duration_ms", time.Since(started).Milliseconds()))
		} else {
			logging.Logger(ctx, s.logger).InfoContext(ctx, "local pack generated", "cache_key", scope, "response_bytes", file.bytes,
				"duration_ms", time.Since(started).Milliseconds(), "pack_store_result", packStore)
		}
	}()
	view, err := lease.View(ctx, snapshot, roots)
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(view)
	phase = "response_spool"
	output, err := os.CreateTemp(s.store.Tmp, "response-")
	if err != nil {
		return nil, err
	}
	file = &responseFile{file: output, budget: s.responses, limit: s.config.MaxResponseBytes}
	pending := file
	failed := true
	defer func() {
		if failed {
			pending.Close()
		}
	}()
	phase = "upload_pack"
	if err := s.git.Run(ctx, view, bytes.NewReader(data), file, map[string]string{"GIT_PROTOCOL": "version=2"}, "-c", "uploadpack.allowFilter=true", "upload-pack", "--stateless-rpc", view); err != nil {
		return nil, err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if key != nil {
		phase = "pack_store"
		packStore = "SKIPPED"
		if stored, err := lease.StorePack(key, output.Name()); err != nil {
			packStore = "FAILED"
			logging.Logger(ctx, s.logger).LogAttrs(ctx, slog.LevelDebug, "response pack cache store failed", logging.ErrorAttrs(err)...)
		} else if stored {
			packStore = "STORED"
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
// shape, or a generation failure hydration may repair); the third reports a
// response-pack hit. A non-nil error on a request that was not served means
// the failure is one hydration cannot repair: the caller passes through.
func (s *Server) serveShared(ctx context.Context, lease *cache.Lease, scope string, key []byte, failure string, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, bool, bool, error) {
	if key != nil {
		if file := s.storedUnderLease(ctx, scope, key, lease); file != nil {
			return file, true, true, nil
		}
	}
	roots := unique(fetch.Wants)
	if !lease.Complete(ctx, roots) {
		return nil, false, false, nil
	}
	// Haves outside the authorized roots are dropped from the request
	// instead of failing it; the exclusive path reports what cannot be
	// classified at all.
	request, err := localRequest(ctx, lease, fetch, roots, data)
	if err != nil {
		return nil, false, false, nil
	}
	if fetch.IncludeTag {
		tags, err := tagRoots(ctx, lease, snapshot, fetch.Wants)
		if err != nil {
			return nil, false, false, nil
		}
		roots = unique(append(roots, tags...))
		if !lease.Complete(ctx, roots) {
			return nil, false, false, nil
		}
	}
	if key != nil {
		// Shared readers never block each other, so identical cacheable
		// requests on a warm scope would each run upload-pack. One reader
		// leads the generation; the rest wait and stream its entry.
		done, lead := s.flights.waitOrLead(scope + "/" + hex.EncodeToString(key))
		if lead == nil {
			s.flightWaits.Add(1)
			started := time.Now()
			select {
			case <-done:
				logging.Logger(ctx, s.logger).DebugContext(ctx, "response pack generation wait completed", "cache_key", scope, "queue_wait_ms", time.Since(started).Milliseconds())
			case <-ctx.Done():
				logging.Failure(ctx, s.logger, "response pack generation wait canceled", ctx.Err(), slog.String("cache_key", scope), slog.Int64("queue_wait_ms", time.Since(started).Milliseconds()))
				return nil, false, false, nil
			}
			if file := s.storedUnderLease(ctx, scope, key, lease); file != nil {
				return file, true, true, nil
			}
			// The leader failed; this reader generates without a new flight.
			logging.Logger(ctx, s.logger).DebugContext(ctx, "response pack generation retried", "cache_key", scope, "reason", "entry_unavailable_after_wait")
		} else {
			// A leader may have published between the entry lookup above
			// and this registration.
			if file := s.storedUnderLease(ctx, scope, key, lease); file != nil {
				lead()
				return file, true, true, nil
			}
			defer lead()
		}
	}
	file, err := s.generateLocal(ctx, lease, scope, snapshot, roots, request, key)
	if err != nil {
		if unrecoverableGeneration(err) {
			// A response that outgrew its limits is not made smaller by
			// hydrating: skip the exclusive retry. Only the size limit is
			// deterministic; the spool budget frees with other transfers.
			var limit responseLimitError
			if errors.As(err, &limit) && limit.reason == "response_size_limit" {
				s.generations.fail(failure)
			}
			return nil, false, false, err
		}
		return nil, false, false, nil
	}
	return file, true, false, nil
}

func (s *Server) prepareObjects(ctx context.Context, identity, site string, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte, ensure func(context.Context, *cache.Lease, []string) (bool, error)) (file *responseFile, status string, packHit bool, err error) {
	started := time.Now()
	phase := "pack_lookup"
	ctx, cancel := context.WithTimeout(ctx, s.gitTimeout)
	defer cancel()
	scope := s.store.Key(identity)
	defer func() {
		// An expected outcome of an earlier failure, not a new one.
		deferred := errors.Is(err, errGenerationBackoff) || errors.Is(err, cache.ErrHydrationBackoff)
		stage := phase
		if errors.Is(err, errGenerationBackoff) {
			stage = "generation_backoff"
		} else if errors.Is(err, cache.ErrHydrationBackoff) {
			stage = "hydration_backoff"
		}
		attrs := []slog.Attr{slog.String("phase", stage), slog.String("cache_key", scope),
			slog.Int("want_count", len(fetch.Wants)), slog.Int("have_count", len(fetch.Haves)), slog.Int64("duration_ms", time.Since(started).Milliseconds())}
		if err != nil {
			if deferred {
				logging.Logger(ctx, s.logger).LogAttrs(ctx, slog.LevelDebug, "local fetch preparation deferred", attrs...)
				return
			}
			logging.Failure(ctx, s.logger, "local fetch preparation failed", err, attrs...)
			return
		}
		responseBytes := file.bytes
		if file.persist {
			responseBytes = file.size
		}
		if status == "HIT" {
			logging.Logger(ctx, s.logger).DebugContext(ctx, "cache objects reused", "cache_key", scope, "root_count", len(fetch.Wants))
		}
		attrs = append(attrs, slog.String("cache_status", status), slog.Bool("pack_cache_hit", packHit), slog.Int64("response_bytes", responseBytes))
		logging.Logger(ctx, s.logger).LogAttrs(ctx, slog.LevelInfo, "local fetch prepared", attrs...)
	}()
	key := s.packCacheKey(identity, fetch, data, snapshot)
	// A generation is remembered by its response key; a request that has
	// none — it carries client haves — by a digest of its bytes.
	failure := generationKey(scope, key)
	if key == nil {
		digest := sha256.Sum256(protocol.FilterRequest(data, protocol.AgentLine))
		failure = generationKey(scope, digest[:])
	}
	if key != nil {
		// Warm responses skip the repository gate entirely: pin the scope
		// against eviction, stream the stored file and never start a Git
		// process. A missing scope or entry falls through to the gated
		// path, which also collapses concurrent cold requests into a
		// single generation.
		if unpin := s.store.Pin(scope); unpin != nil {
			if stored, err := s.store.LookupPack(scope, key); err == nil && stored != nil {
				s.packHits.Add(1)
				// The hit carries a snapshot from its authorization probe like
				// any served fetch, so it arms the reconciliation too — before
				// the pin goes, for the same reason as the gated paths.
				s.served(scope, site, snapshot)
				return s.warmResponse(ctx, scope, key, stored, unpin), "HIT", true, nil
			}
			unpin()
		}
	}
	// A generation that failed for this request recently is not attempted
	// again until its backoff passes.
	if s.generations.blocked(failure) {
		phase = "generation_backoff"
		return nil, "", false, errGenerationBackoff
	}
	// Shared read path: when every object the request needs is already
	// cached, generation runs under a reader lease in parallel with other
	// readers of the same scope. Storage refs catch up on a background
	// writer instead of blocking the response.
	phase = "shared_lease"
	leaseStarted := time.Now()
	if lease, err := s.store.AcquireShared(ctx, scope); err == nil {
		logging.Logger(ctx, s.logger).DebugContext(ctx, "repository lease acquired", "queue_wait_ms", time.Since(leaseStarted).Milliseconds(), "cache_key", scope, "lease_mode", "shared")
		phase = "shared_prepare"
		file, served, packHit, abort := s.serveShared(ctx, lease, scope, key, failure, fetch, snapshot, data)
		if served {
			// Every locally served fetch carries a complete live snapshot
			// from its authorization probe; debounced, idle-only
			// reconciliation follows it in the background. It is armed while
			// the lease is still held: a run preempted by this request waits
			// for the scope to go idle, and must find this snapshot when it
			// does, not the older one it was started for.
			s.served(scope, site, snapshot)
		}
		lease.Release()
		if served {
			s.generations.done(failure)
			return file, "HIT", packHit, nil
		}
		if abort != nil {
			return nil, "", false, abort
		}
	}
	phase = "lease"
	leaseStarted = time.Now()
	lease, err := s.store.Acquire(ctx, scope)
	if err != nil {
		return nil, "", false, err
	}
	defer lease.Release()
	logging.Logger(ctx, s.logger).DebugContext(ctx, "repository lease acquired", "queue_wait_ms", time.Since(leaseStarted).Milliseconds(), "cache_key", scope, "lease_mode", "exclusive")
	if key != nil {
		// Convert a stored hit's lease into a streaming pin: the response
		// outlives the gate, but eviction cannot remove its scope mid-stream.
		if file := s.storedUnderLease(ctx, scope, key, lease); file != nil {
			s.served(scope, site, snapshot)
			return file, "HIT", true, nil
		}
	}
	phase = "hydrate"
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
	phase = "haves"
	data, err = localRequest(ctx, lease, fetch, roots, data)
	if err != nil {
		return nil, "", false, err
	}
	if fetch.FullFetch() {
		phase = "sync_refs"
		if err := lease.SyncRefs(ctx, snapshot); err != nil {
			return nil, "", false, err
		}
	}
	if fetch.IncludeTag {
		phase = "tags"
		tags, err := tagRoots(ctx, lease, snapshot, fetch.Wants)
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
	// A deadline that ran out while queueing says nothing about the
	// response: it is not recorded as a generation failure.
	if err := ctx.Err(); err != nil {
		return nil, "", false, err
	}
	phase = "generate_local"
	file, err = s.generateLocal(ctx, lease, scope, snapshot, roots, data, key)
	if err != nil {
		s.recordGenerationFailure(failure, err)
		return nil, "", false, err
	}
	s.generations.done(failure)
	// For full fetches the inline SyncRefs above already applied the
	// snapshot, and the debounced call no-ops; partial and incremental
	// generations schedule their prune through the background path.
	s.served(scope, site, snapshot)
	status = "MISS"
	if hit {
		status = "HIT"
	}
	return file, status, false, nil
}
