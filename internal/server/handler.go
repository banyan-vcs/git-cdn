package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
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
	started := time.Now()
	finish := func() {}
	authCacheStatus := "NONE"
	r = r.WithContext(logging.WithLogger(r.Context(), s.logger.With("transport", "http", "method", r.Method)))
	defer func() {
		defer func() { finish() }()
		panicked := recover()
		if r.URL.Path != "/metrics" {
			status := int(output.status.Load())
			if status == 0 {
				status = http.StatusOK
				if panicked != nil {
					status = http.StatusInternalServerError
				}
			}
			level := slog.LevelInfo
			if status >= 500 || panicked != nil {
				level = slog.LevelWarn
			}
			var requestErr error
			if output.ctx != nil {
				requestErr = output.ctx.Err()
				if errors.Is(requestErr, context.Canceled) {
					level = slog.LevelDebug
				}
			}
			cacheStatus := output.Header().Get("X-GitCDN-Cache-Status")
			if cacheStatus == "" {
				cacheStatus = "NONE"
			}
			attrs := []slog.Attr{slog.Int("status", status), slog.Int64("response_bytes", output.bytes.Load()),
				slog.Int64("duration_ms", time.Since(started).Milliseconds()), slog.String("cache_status", cacheStatus), slog.Bool("panicked", panicked != nil)}
			packStatus := output.Header().Get("X-GitCDN-Pack-Cache")
			if packStatus == "" {
				packStatus = "NONE"
			}
			attrs = append(attrs, slog.String("pack_cache_status", packStatus), slog.String("auth_cache_status", authCacheStatus))
			attrs = append(attrs, logging.ErrorAttrs(requestErr)...)
			logging.Logger(r.Context(), s.logger).LogAttrs(r.Context(), level, "repository request completed", attrs...)
		}
		if panicked != nil {
			panic(panicked)
		}
	}()
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
		fmt.Fprintf(w, "gitcdn_gcs_total %d\ngitcdn_gc_failures_total %d\ngitcdn_gc_skipped_total %d\n", stats.GCs, stats.GCFailed, stats.GCSkipped)
		fmt.Fprintf(w, "gitcdn_reconciles_total %d\ngitcdn_roots_retired_total %d\n", stats.Reconciles, stats.RootsRetired)
		fmt.Fprintf(w, "gitcdn_active_requests %d\ngitcdn_rejected_requests_total %d\ngitcdn_temp_response_bytes %d\ngitcdn_temp_response_max_bytes %d\n", s.requests.active(), s.rejected.Load(), s.responses.bytes(), s.config.MaxTempResponseBytes)
		fmt.Fprintf(w, "gitcdn_pack_cache_hits_total %d\ngitcdn_pack_cache_stores_total %d\n", s.packHits.Load(), s.packStores.Load())
		fmt.Fprintf(w, "gitcdn_response_fanout_subscribers_total %d\ngitcdn_response_fanout_detached_total %d\n", s.fanoutSubs.Load(), s.fanoutDetached.Load())
		fmt.Fprintf(w, "gitcdn_generation_flight_waits_total %d\n", s.flightWaits.Load())
		fmt.Fprintf(w, "gitcdn_auth_probe_cache_hits_total %d\ngitcdn_auth_probes_merged_total %d\n", s.probeHits.Load(), s.probeMerged.Load())
		return
	}
	if !s.requests.acquire(time.Now()) {
		s.rejected.Add(1)
		w.Header().Set("Retry-After", "1")
		http.Error(w, "Git request capacity exhausted", http.StatusServiceUnavailable)
		return
	}
	defer s.requests.release()
	r, finish = output.requestDeadline(r, s.requestTimeout)
	rt, err := s.resolve(r)
	if err != nil {
		logging.Failure(r.Context(), s.logger, "repository request rejected", err, slog.String("phase", "route"))
		http.Error(w, "invalid upstream or repository route", http.StatusBadRequest)
		return
	}
	r = r.WithContext(logging.WithLogger(r.Context(), s.logger.With("transport", "http", "method", r.Method,
		"upstream", rt.id, "upstream_host", rt.base.Host, "repository", rt.repo, "endpoint", rt.endpoint)))
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
		logging.Failure(r.Context(), s.logger, "repository request rejected", err, slog.String("phase", "request_body"))
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
		logging.Failure(r.Context(), s.logger, "repository request rejected", err, slog.String("phase", "decode_body"))
		http.Error(w, "invalid or oversized encoded request", http.StatusBadRequest)
		return
	}
	fetch, err := protocol.ParseFetch(data)
	if err != nil {
		logging.Failure(r.Context(), s.logger, "repository request rejected", err, slog.String("phase", "parse_fetch"))
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
	snapshot, response, authCached, err := s.probe(r, rt)
	authCacheStatus = "LIVE"
	if authCached {
		authCacheStatus = "HIT"
	}
	if err != nil {
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
	file, status, packHit, err := s.prepare(r, rt, fetch, snapshot, data)
	if err != nil {
		if r.Context().Err() == nil {
			logging.Logger(r.Context(), s.logger).LogAttrs(r.Context(), slog.LevelDebug, "local response generation failed; falling back to upstream", logging.ErrorAttrs(err)...)
			s.proxy(w, r, rt, "local-unavailable")
		}
		return
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		logging.Failure(r.Context(), s.logger, "local response unavailable", err, slog.String("phase", "response_stat"))
		s.proxy(w, r, rt, "local-unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/x-git-upload-pack-result")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-GitCDN-Cache-Status", status)
	if s.probes.ttl > 0 {
		if authCached {
			w.Header().Set("X-GitCDN-Auth-Cache", "HIT")
		} else {
			w.Header().Set("X-GitCDN-Auth-Cache", "LIVE")
		}
	}
	if s.store.PackCacheBytes() > 0 {
		if packHit {
			w.Header().Set("X-GitCDN-Pack-Cache", "HIT")
		} else {
			w.Header().Set("X-GitCDN-Pack-Cache", "MISS")
		}
	}
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	s.local.Add(1)
	if _, err := io.Copy(w, file); err != nil {
		logging.Logger(r.Context(), s.logger).LogAttrs(r.Context(), slog.LevelDebug, "client transfer interrupted", logging.ErrorAttrs(err)...)
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
