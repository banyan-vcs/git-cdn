package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

func (s *Server) prepare(r *http.Request, rt route, fetch protocol.Fetch, snapshot protocol.Snapshot, data []byte) (*responseFile, string, error) {
	ctx, cancel := context.WithTimeout(r.Context(), s.gitTimeout)
	defer cancel()
	headers := s.headers(r)
	credential, _ := json.Marshal(headers)
	identity := rt.base.String() + "/" + strings.TrimSuffix(rt.repo, ".git") + ".git:sha1"
	lease, err := s.store.Acquire(ctx, s.store.Key(identity, string(credential)))
	if err != nil {
		return nil, "", err
	}
	defer lease.Release()
	origin := rt.target(rt.repo).String()
	roots := unique(fetch.Wants)
	fetchCtx, fetchCancel := context.WithTimeout(ctx, s.fetchTimeout)
	hit, err := lease.Ensure(fetchCtx, origin, headers, roots)
	fetchCancel()
	if err != nil {
		return nil, "", err
	}
	if !hit {
		s.hydrated.Add(1)
	}
	for _, have := range unique(fetch.Haves) {
		if !lease.Ancestor(ctx, have, roots) {
			return nil, "", errors.New("have is outside authorized roots")
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
				return nil, "", errors.New("tag validation exceeds local budget")
			}
			target := ref.OID
			if ref.Peeled != "" {
				target = ref.Peeled
			}
			if lease.Ancestor(ctx, target, fetch.Wants) {
				roots = append(roots, ref.OID)
			}
			if len(roots) > 128 {
				return nil, "", errors.New("too many tag roots")
			}
		}
		roots = unique(roots)
		fetchCtx, fetchCancel := context.WithTimeout(ctx, s.fetchTimeout)
		tagHit, err := lease.Ensure(fetchCtx, origin, headers, roots)
		fetchCancel()
		if err != nil {
			return nil, "", err
		}
		if !tagHit {
			s.hydrated.Add(1)
			hit = false
		}
	}
	view, err := lease.View(ctx, snapshot, roots)
	if err != nil {
		return nil, "", err
	}
	defer os.RemoveAll(view)
	output, err := os.CreateTemp(s.store.Tmp, "response-")
	if err != nil {
		return nil, "", err
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
		return nil, "", err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, "", err
	}
	failed = false
	status := "MISS"
	if hit {
		status = "HIT"
	}
	return file, status, nil
}
