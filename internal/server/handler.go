package server

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
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
	if r.Body != nil && r.Body != http.NoBody {
		r.Body = &idleRequestBody{ReadCloser: r.Body, output: output}
	}
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
		recovered := recover()
		panicked := recovered
		// ReverseProxy re-raises http.ErrAbortHandler once a committed
		// upstream response could not be completed — the client stopped
		// reading or the upstream stopped sending. The server suppresses
		// that panic and closes the connection; it is an interrupted
		// transfer, not a fault to page on, and is logged as such with
		// the side that failed.
		if failure, ok := recovered.(error); ok && errors.Is(failure, http.ErrAbortHandler) {
			panicked = nil
			side, err := output.interrupted()
			attrs := append([]slog.Attr{slog.String("side", side), slog.Int64("response_bytes", output.bytes.Load())}, logging.ErrorAttrs(err)...)
			logging.Logger(r.Context(), s.logger).LogAttrs(r.Context(), slog.LevelInfo, "upstream passthrough interrupted", attrs...)
		}
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
		if recovered != nil {
			panic(recovered)
		}
	}()
	if !s.secureRequest(r) {
		http.Error(w, "HTTPS or a trusted TLS proxy is required", http.StatusForbidden)
		return
	}
	if r.URL.Path == "/metrics" && r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		s.writeMetrics(w)
		return
	}
	// Overload queues briefly instead of rejecting: Git never retries a 503,
	// so a burst that exceeds the limits for a moment would fail the clone.
	if !s.requests.acquire(r.Context()) {
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
		if errors.Is(err, errProbeUnusable) {
			// The origin answered but its refs cannot be parsed or exceed
			// max_refs_bytes. Cached data stays off limits — nothing
			// authorized this request — and the fetch itself goes upstream,
			// which served the same client fine directly.
			s.proxy(w, r, rt, "probe-unusable")
			return
		}
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
