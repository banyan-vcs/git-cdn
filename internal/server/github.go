package server

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHub's Git endpoint also challenges nonexistent repositories. An absent
// owner is independent evidence; a repository API 404 alone could hide a private repo.
func (s *Server) normalizeGitHubNotFound(r *http.Request, rt route, response *http.Response) bool {
	if response.StatusCode != http.StatusUnauthorized || rt.base.Scheme != "https" || !strings.EqualFold(rt.base.Hostname(), "github.com") || (rt.base.Port() != "" && rt.base.Port() != "443") || strings.Trim(rt.base.Path, "/") != "" {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(rt.repo, ".git"), "/")
	if len(parts) != 2 {
		return false
	}
	ctx, cancel := context.WithTimeout(r.Context(), min(s.tune.Load().AuthTimeout, 5*time.Second))
	defer cancel()
	api := *s.githubAPI
	api.Path = strings.TrimSuffix(api.Path, "/") + "/users/" + parts[0]
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, api.String(), nil)
	if err != nil {
		return false
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "git-cdn/0.1")
	// This public owner lookup must never receive the caller's Git credentials.
	metadata, err := s.client.Do(request)
	if err != nil {
		return false
	}
	defer metadata.Body.Close()
	if metadata.StatusCode != http.StatusNotFound {
		return false
	}
	contentType, _, _ := mime.ParseMediaType(metadata.Header.Get("Content-Type"))
	if contentType != "application/json" {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(metadata.Body, 65537))
	if err != nil || len(data) > 65536 {
		return false
	}
	var result struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &result) != nil || result.Message != "Not Found" {
		return false
	}
	response.Body.Close()
	body := "repository not found\n"
	response.Body = io.NopCloser(strings.NewReader(body))
	response.StatusCode = http.StatusNotFound
	response.Status = "404 Not Found"
	response.ContentLength = int64(len(body))
	response.Header.Del("WWW-Authenticate")
	response.Header.Del("Content-Encoding")
	response.Header.Del("ETag")
	response.Header.Set("Content-Type", "text/plain; charset=utf-8")
	response.Header.Set("Content-Length", strconv.Itoa(len(body)))
	response.Header.Set("Cache-Control", "no-store")
	return true
}
