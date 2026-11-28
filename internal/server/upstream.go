package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"mime"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

func (s *Server) headers(r *http.Request) [][2]string {
	var headers [][2]string
	for _, name := range s.config.AuthHeaders {
		for _, value := range r.Header.Values(name) {
			headers = append(headers, [2]string{name, value})
		}
	}
	return headers
}

// errProbeUnusable marks an origin answer that cannot authorize anything:
// a 200 whose body is not a parseable ref advertisement, exceeds
// max_refs_bytes or is not a Git response. Transport failures and denials
// are reported differently; only this one lets the request pass through.
var errProbeUnusable = errors.New("upstream refs response is not usable")

func (s *Server) discover(r *http.Request, rt route) (snapshot protocol.Snapshot, upstreamResponse *http.Response, err error) {
	observedAt := time.Now()
	status, responseBytes := 0, int64(0)
	ctx, cancel := context.WithTimeout(r.Context(), s.tune.Load().AuthTimeout)
	defer cancel()
	defer func() {
		attrs := []slog.Attr{slog.String("phase", "refs"), slog.Int("status", status), slog.Int64("response_bytes", responseBytes),
			slog.Int64("duration_ms", time.Since(observedAt).Milliseconds()), slog.Int("ref_count", len(snapshot.Refs))}
		if err != nil {
			// The origin's answer is recorded for the operator; waiters on a
			// merged probe only need the class to make the same decision.
			logging.Failure(ctx, s.logger, "upstream refs probe failed", err, attrs...)
		} else {
			logging.Logger(ctx, s.logger).LogAttrs(ctx, slog.LevelInfo, "upstream refs refreshed", attrs...)
		}
	}()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rt.target(rt.repo+"/git-upload-pack").String(), bytes.NewReader(protocol.LSRefsRequest()))
	if err != nil {
		return protocol.Snapshot{}, nil, err
	}
	for _, header := range s.headers(r) {
		request.Header.Add(header[0], header[1])
	}
	request.Header.Set("Git-Protocol", "version=2")
	request.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	request.Header.Set("Accept-Encoding", "identity")
	request.Header.Set("User-Agent", "git-cdn/0.1")
	response, err := s.client.Do(request)
	if err != nil {
		return protocol.Snapshot{}, nil, err
	}
	defer response.Body.Close()
	status = response.StatusCode
	data, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxRefsBytes+1))
	responseBytes = int64(len(data))
	if err != nil || int64(len(data)) > s.config.MaxRefsBytes {
		return protocol.Snapshot{}, nil, fmt.Errorf("%w: oversized or incomplete refs response", errProbeUnusable)
	}
	if response.StatusCode != http.StatusOK {
		if s.normalizeGitHubNotFound(r, rt, response) {
			data, err = io.ReadAll(response.Body)
			if err != nil {
				return protocol.Snapshot{}, nil, err
			}
		}
		response.Body = io.NopCloser(bytes.NewReader(data))
		response.ContentLength = int64(len(data))
		return protocol.Snapshot{}, response, nil
	}
	contentType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if contentType != "application/x-git-upload-pack-result" {
		return protocol.Snapshot{}, nil, fmt.Errorf("%w: unexpected content type %q", errProbeUnusable, contentType)
	}
	if data, err = decodeBody(data, response.Header.Get("Content-Encoding"), s.config.MaxRefsBytes); err != nil {
		return protocol.Snapshot{}, nil, fmt.Errorf("%w: %v", errProbeUnusable, err)
	}
	snapshot, err = protocol.ParseRefs(data)
	if err != nil {
		return protocol.Snapshot{}, nil, fmt.Errorf("%w: %v", errProbeUnusable, err)
	}
	snapshot.ObservedAt = observedAt
	return snapshot, nil, nil
}

var proxyHeaders = []string{"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "Content-Encoding", "Git-Protocol", "User-Agent", "Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Cache-Control", "Pragma"}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, rt route, reason string) {
	s.proxied.Add(1)
	started := time.Now()
	status := 0
	var body *countedBody
	defer func() {
		var received int64
		if body != nil {
			received = body.bytes
		}
		logging.Logger(r.Context(), s.logger).InfoContext(r.Context(), "upstream passthrough completed", "reason", reason,
			"status", status, "upstream_response_bytes", received, "duration_ms", time.Since(started).Milliseconds())
	}()
	p := &httputil.ReverseProxy{
		Transport: s.transport,
		// ReverseProxy's own messages go to the standard library logger,
		// outside the structured stream, and format the raw error. They
		// are silenced here: a body copy error is reported by the caller
		// through slog with the error classified, never spelled out.
		ErrorLog: log.New(io.Discard, "", 0),
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(rt.base)
			pr.Out.URL.Path = rt.target(rt.path).Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = rt.base.Host
			pr.Out.Header = make(http.Header)
			for _, name := range s.proxyAllow {
				for _, value := range r.Header.Values(name) {
					pr.Out.Header.Add(name, value)
				}
			}
		},
		ModifyResponse: func(response *http.Response) error {
			s.normalizeGitHubNotFound(r, rt, response)
			status = response.StatusCode
			body = &countedBody{ReadCloser: response.Body}
			response.Body = body
			if output, ok := w.(*idleResponseWriter); ok {
				output.upstreamErr = body.readErr
			}
			response.Header.Set("Cache-Control", "no-store")
			response.Header.Set("X-GitCDN-Cache-Status", "BYPASS")
			if location := rewriteLocation(rt, response); location != "" {
				response.Header.Set("Location", location)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			status = http.StatusBadGateway
			logging.Failure(r.Context(), s.logger, "upstream proxy failed", err, slog.String("phase", "passthrough"), slog.String("reason", reason))
			http.Error(w, "upstream proxy failed", http.StatusBadGateway)
		},
	}
	p.ServeHTTP(w, r)
}

// countedBody counts the bytes streamed from the origin and keeps the
// first read error, which ReverseProxy otherwise reports only by aborting.
type countedBody struct {
	io.ReadCloser
	bytes int64
	err   error
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.bytes += int64(n)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}

func (b *countedBody) readErr() error { return b.err }

func copyHeaders(dst, src http.Header) {
	blocked := map[string]bool{"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true, "Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true}
	for _, connection := range src.Values("Connection") {
		for _, name := range strings.Split(connection, ",") {
			blocked[http.CanonicalHeaderKey(strings.TrimSpace(name))] = true
		}
	}
	for name, values := range src {
		if !blocked[http.CanonicalHeaderKey(name)] {
			dst[name] = append([]string(nil), values...)
		}
	}
}
