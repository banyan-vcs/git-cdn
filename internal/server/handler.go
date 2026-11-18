package server

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/protocol"
)

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	output := &idleResponseWriter{ResponseWriter: w, controller: controller, timeout: s.writeIdleTimeout}
	w = output
	w.Header().Set("Cache-Control", "no-store")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"status":"ok"}`)
		return
	}
	if !s.secureRequest(r) {
		http.Error(w, "HTTPS or a trusted TLS proxy is required", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/metrics" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "gitcdn_local_requests_total %d\ngitcdn_hydrations_total %d\ngitcdn_passthrough_requests_total %d\ngitcdn_auth_denied_total %d\n", s.local.Load(), s.hydrated.Load(), s.proxied.Load(), s.authDenied.Load())
		stats := s.store.Stats()
		fmt.Fprintf(w, "gitcdn_cache_bytes %d\ngitcdn_cache_max_bytes %d\ngitcdn_cache_reserved_bytes %d\ngitcdn_cache_entries %d\ngitcdn_cache_evictions_total %d\ngitcdn_cache_evicted_bytes_total %d\n", stats.Bytes, stats.MaxBytes, stats.ReservedBytes, stats.Entries, stats.Evictions, stats.EvictedBytes)
		fmt.Fprintf(w, "gitcdn_repacks_total %d\ngitcdn_repack_failures_total %d\ngitcdn_repack_skipped_total %d\n", stats.Repacks, stats.RepackFailed, stats.RepackSkipped)
		fmt.Fprintf(w, "gitcdn_active_requests %d\ngitcdn_rejected_requests_total %d\ngitcdn_temp_response_bytes %d\ngitcdn_temp_response_max_bytes %d\n", s.requests.active(), s.rejected.Load(), s.responses.bytes(), s.config.MaxTempResponseBytes)
		return
	}
	if !s.requests.acquire(time.Now()) {
		s.rejected.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Git request capacity exhausted", http.StatusServiceUnavailable)
		return
	}
	defer s.requests.release()
	r, finish := output.requestDeadline(r, s.requestTimeout)
	defer finish()
	rt, err := s.resolve(r)
	if err != nil {
		http.Error(w, "invalid upstream or repository route", http.StatusBadRequest)
		return
	}
	if !s.credentialHostMatches(r, rt) {
		http.Error(w, "credentials are not accepted for this upstream on this host", http.StatusMisdirectedRequest)
		return
	}
	if r.Method != http.MethodPost || rt.endpoint != "git-upload-pack" || !protocol.IsV2(r.Header.Get("Git-Protocol")) {
		s.proxy(w, r, rt, "route")
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.config.MaxRequestBytes))
	if err != nil {
		http.Error(w, "request body exceeds limit or cannot be read", http.StatusRequestEntityTooLarge)
		return
	}
	r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(raw))
	encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding")))
	if encoding != "" && encoding != "identity" && encoding != "gzip" {
		s.proxy(w, r, rt, "content-encoding")
		return
	}
	data, err := decodeBody(raw, r.Header.Get("Content-Encoding"), s.config.MaxRequestBytes)
	if err != nil {
		http.Error(w, "invalid or oversized encoded request", http.StatusBadRequest)
		return
	}
	fetch, err := protocol.ParseFetch(data)
	if err != nil {
		http.Error(w, "invalid Git packet framing", http.StatusBadRequest)
		return
	}
	if fetch.Command != "fetch" || !fetch.Supported || len(fetch.Wants) == 0 || r.URL.RawQuery != "" {
		s.proxy(w, r, rt, "unsupported")
		return
	}
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mediaType != "application/x-git-upload-pack-request" {
		s.proxy(w, r, rt, "content-type")
		return
	}
	snapshot, response, err := s.discover(r, rt)
	if err != nil {
		s.logger.Warn("upstream refs probe failed", "upstream", rt.id)
		http.Error(w, "upstream refs probe failed", http.StatusBadGateway)
		return
	}
	if response != nil {
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden || response.StatusCode == http.StatusNotFound {
			s.authDenied.Add(1)
		}
		defer response.Body.Close()
		copyHeaders(w.Header(), response.Header)
		if location := rewriteLocation(rt, response); location != "" {
			w.Header().Set("Location", location)
		}
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(response.StatusCode)
		io.Copy(w, response.Body)
		return
	}
	if !snapshot.Allows(fetch.Wants) {
		s.proxy(w, r, rt, "unadvertised-oid")
		return
	}
	file, status, err := s.prepare(r, rt, fetch, snapshot, data)
	if err != nil {
		if r.Context().Err() == nil {
			s.proxy(w, r, rt, "local-unavailable")
		}
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		s.proxy(w, r, rt, "local-unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-GitCDN-Cache-Status", status)
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	s.local.Add(1)
	if _, err := io.Copy(w, file); err != nil {
		s.logger.Debug("client transfer interrupted", "upstream", rt.id)
	}
}

func decodeBody(raw []byte, encoding string, limit int64) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
		return raw, nil
	case "gzip":
		g, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer g.Close()
		data, err := io.ReadAll(io.LimitReader(g, limit+1))
		if err != nil || int64(len(data)) > limit {
			return nil, errors.New("invalid or oversized gzip body")
		}
		return data, nil
	default:
		return nil, errors.New("unsupported content encoding")
	}
}
