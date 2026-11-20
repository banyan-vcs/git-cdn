package server

import (
	"bytes"
	"context"
	"errors"
	"github.com/chiyutianyi/git-cdn/internal/cache"
	"io"
	"net/http"
	"os"
	"strings"

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

func (s *Server) prepare(r *http.Request, rt route, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, string, bool, error) {
	headers := s.headers(r)
	identity := rt.base.String() + "/" + strings.TrimSuffix(rt.repo, ".git") + ".git:sha1"
	return s.prepareObjects(r.Context(), identity, fetch, snapshot, data, func(ctx context.Context, l *cache.Lease, roots []string) (bool, error) {
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

func (s *Server) prepareObjects(ctx context.Context, identity string, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte, ensure func(context.Context, *cache.Lease, []string) (bool, error)) (*responseFile, string, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.gitTimeout)
	defer cancel()
	lease, err := s.store.Acquire(ctx, s.store.Key(identity))
	if err != nil {
		return nil, "", false, err
	}
	defer lease.Release()
	key := s.packCacheKey(identity, fetch, data, snapshot)
	if key != nil {
		if stored, err := lease.LookupPack(key); err == nil && stored != nil {
			s.packHits.Add(1)
			return &responseFile{file: stored, budget: s.responses, limit: s.config.MaxResponseBytes, persist: true}, "HIT", true, nil
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
		tagCount := 0
		for _, ref := range snapshot.Refs {
			if !strings.HasPrefix(ref.Name, "refs/tags/") {
				continue
			}
			tagCount++
			if tagCount > 128 || ctx.Err() != nil {
				return nil, "", false, errors.New("tag validation exceeds local budget")
			}
			target := ref.OID
			if ref.Peeled != "" {
				target = ref.Peeled
			}
			if lease.Ancestor(ctx, target, fetch.Wants) {
				roots = append(roots, ref.OID)
			}
			if len(roots) > 128 {
				return nil, "", false, errors.New("too many tag roots")
			}
		}
		roots = unique(roots)
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
