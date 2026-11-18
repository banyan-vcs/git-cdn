package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httputil"
	"strings"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/protocol"
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

func (s *Server) discover(r *http.Request, rt route) (protocol.Snapshot, *http.Response, error) {
	observedAt := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), s.authTimeout)
	defer cancel()
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
	data, err := io.ReadAll(io.LimitReader(response.Body, s.config.MaxRefsBytes+1))
	if err != nil || int64(len(data)) > s.config.MaxRefsBytes {
		return protocol.Snapshot{}, nil, errors.New("invalid or oversized upstream refs response")
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
		return protocol.Snapshot{}, nil, errors.New("upstream did not return a Git response")
	}
	data, err = decodeBody(data, response.Header.Get("Content-Encoding"), s.config.MaxRefsBytes)
	if err != nil {
		return protocol.Snapshot{}, nil, err
	}
	snapshot, err := protocol.ParseRefs(data)
	snapshot.ObservedAt = observedAt
	return snapshot, nil, err
}

var proxyHeaders = []string{"Accept", "Accept-Encoding", "Accept-Language", "Content-Type", "Content-Encoding", "Git-Protocol", "User-Agent", "Range", "If-Match", "If-None-Match", "If-Modified-Since", "If-Unmodified-Since", "Cache-Control", "Pragma"}

func (s *Server) proxy(w http.ResponseWriter, r *http.Request, rt route, reason string) {
	s.proxied.Add(1)
	p := &httputil.ReverseProxy{
		Transport: s.transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(rt.base)
			pr.Out.URL.Path = rt.target(rt.path).Path
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = r.URL.RawQuery
			pr.Out.Host = rt.base.Host
			pr.Out.Header = make(http.Header)
			for _, name := range append(append([]string{}, proxyHeaders...), s.config.AuthHeaders...) {
				for _, value := range r.Header.Values(name) {
					pr.Out.Header.Add(name, value)
				}
			}
		},
		ModifyResponse: func(response *http.Response) error {
			s.normalizeGitHubNotFound(r, rt, response)
			response.Header.Set("Cache-Control", "no-store")
			response.Header.Set("X-GitCDN-Cache-Status", "BYPASS")
			if location := rewriteLocation(rt, response); location != "" {
				response.Header.Set("Location", location)
			}
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.logger.Warn("upstream proxy failed", "upstream", rt.id, "reason", reason)
			http.Error(w, "upstream proxy failed", http.StatusBadGateway)
		},
	}
	p.ServeHTTP(w, r)
}

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
