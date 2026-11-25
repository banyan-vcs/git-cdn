package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// probeCacheLimit bounds credential-scoped probe entries by count. Each
// entry holds a whole ref snapshot — kilobytes for a small repository,
// megabytes for one with many refs — so probeCacheRefs bounds the refs
// held together as well.
const (
	probeCacheLimit = 8192
	probeCacheRefs  = 1 << 20
)

// A probeCache collapses concurrent ls-refs authorization probes that share
// a repository and credential into one upstream request, and optionally
// reuses a successful probe for a short configured lifetime.
//
// Merging in-flight probes preserves live authorization: waiters receive the
// leader's fresh result for the same instant, so a clone storm costs one
// probe instead of one probe per request. Lifetime reuse is an explicit
// configured tradeoff: authorization degrades from live to near-live, and a
// revoked credential stays readable for at most that lifetime. Only
// successful probes are reused — denials always re-probe — and entries live
// in memory only: credentials appear solely as a hash in the key and never
// touch disk.
type probeCache struct {
	ttl     time.Duration
	mu      sync.Mutex
	entries map[string]probeEntry
	flights map[string]*probeFlight
	// refs totals the ref snapshots held by lifetime entries.
	refs int
	// stop ends the janitor that expires lifetime entries on its own.
	stop chan struct{}
}

type probeEntry struct {
	snapshot protocol.Snapshot
	expires  time.Time
}

// probeFlight carries one in-flight probe's outcome to its waiters.
type probeFlight struct {
	done     chan struct{}
	snapshot protocol.Snapshot
	response *replayedResponse
	err      error
}

// replayedResponse captures an upstream denial so merged waiters relay the
// leader's exact status, headers and body.
type replayedResponse struct {
	status int
	header http.Header
	body   []byte
}

func (c *probeCache) init(ttl time.Duration) {
	c.ttl = ttl
	c.entries = make(map[string]probeEntry)
	c.flights = make(map[string]*probeFlight)
}

// parseProbeTTL maps the configured lifetime to a non-negative duration;
// zero keeps authorization fully live.
func parseProbeTTL(value string) time.Duration {
	ttl, err := time.ParseDuration(value)
	if err != nil || ttl < 0 {
		return 0
	}
	return ttl
}

func (s *Server) probeKey(rt route, headers [][2]string) string {
	h := sha256.New()
	h.Write([]byte(rt.base.String()))
	h.Write([]byte{0})
	h.Write([]byte(rt.repo))
	for _, header := range headers {
		h.Write([]byte{0})
		h.Write([]byte(header[0]))
		h.Write([]byte{':'})
		h.Write([]byte(header[1]))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// probe resolves the authorization snapshot for a fetch, merging concurrent
// probes with the same repository and credential and reusing recent
// successful probes when a lifetime is configured. The fourth return value
// reports a lifetime-cache hit.
func (s *Server) probe(r *http.Request, rt route) (snapshot protocol.Snapshot, response *http.Response, cached bool, err error) {
	started := time.Now()
	result := "LIVE"
	defer func() {
		attrs := []slog.Attr{slog.String("probe_result", result), slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			slog.Int64("auth_cache_ttl_ms", s.probes.ttl.Milliseconds()), slog.Int("ref_count", len(snapshot.Refs))}
		if response != nil {
			attrs = append(attrs, slog.Int("status", response.StatusCode))
		}
		if err != nil {
			logging.Failure(r.Context(), s.logger, "authorization probe failed", err, attrs...)
		} else {
			logging.Logger(r.Context(), s.logger).LogAttrs(r.Context(), slog.LevelDebug, "authorization probe completed", attrs...)
		}
	}()
	key := s.probeKey(rt, s.headers(r))
	now := time.Now()
	c := &s.probes
	if c.ttl > 0 {
		c.mu.Lock()
		entry, ok := c.entries[key]
		if ok {
			if now.After(entry.expires) {
				delete(c.entries, key)
				c.refs -= len(entry.snapshot.Refs)
			} else {
				c.mu.Unlock()
				s.probeHits.Add(1)
				result = "HIT"
				return entry.snapshot, nil, true, nil
			}
		}
		c.mu.Unlock()
	}
	c.mu.Lock()
	if flight, ok := c.flights[key]; ok {
		c.mu.Unlock()
		result = "MERGED"
		select {
		case <-flight.done:
			s.probeMerged.Add(1)
			snapshot, response, err := flight.replay(rt)
			return snapshot, response, false, err
		case <-r.Context().Done():
			return protocol.Snapshot{}, nil, false, r.Context().Err()
		}
	}
	flight := &probeFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.mu.Unlock()
	// Complete the flight no matter how the probe ends: a wedged flight
	// would block every later probe for this key.
	defer func() {
		close(flight.done)
		c.mu.Lock()
		delete(c.flights, key)
		c.mu.Unlock()
	}()
	// The shared probe must not die with the leader's connection: waiters
	// still need its result after the leader disconnects.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.authTimeout)
	defer cancel()
	snapshot, response, err = s.discover(r.Clone(probeCtx), rt)
	flight.snapshot, flight.err = snapshot, err
	if response != nil {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, s.config.MaxRefsBytes+1))
		if readErr == nil {
			flight.response = &replayedResponse{status: response.StatusCode, header: response.Header.Clone(), body: body}
			response.Body = io.NopCloser(bytes.NewReader(body))
			response.ContentLength = int64(len(body))
		}
	}
	if err == nil && response == nil && c.ttl > 0 {
		s.rememberProbe(key, snapshot)
	}
	return snapshot, response, false, err
}

// rememberProbe stores a successful probe for the configured lifetime.
// Entries past their count or ref budget are dropped; map order decides
// which, which is enough for a cache whose entries all share one lifetime.
func (s *Server) rememberProbe(key string, snapshot protocol.Snapshot) {
	c := &s.probes
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if previous, ok := c.entries[key]; ok {
		c.refs -= len(previous.snapshot.Refs)
		delete(c.entries, key)
	}
	c.entries[key] = probeEntry{snapshot: snapshot, expires: now.Add(c.ttl)}
	c.refs += len(snapshot.Refs)
	for len(c.entries) > probeCacheLimit || c.refs > probeCacheRefs {
		evicted := false
		for k, entry := range c.entries {
			if k == key {
				continue
			}
			delete(c.entries, k)
			c.refs -= len(entry.snapshot.Refs)
			evicted = true
			break
		}
		if !evicted {
			// One snapshot larger than the whole ref budget.
			delete(c.entries, key)
			c.refs -= len(snapshot.Refs)
			break
		}
	}
}

// startJanitor expires lifetime entries once per lifetime. Expiry used to
// wait for a lookup of the same key or for the count limit: after a burst
// across many repositories, every snapshot stayed in memory until one of
// those happened, which for one-off repositories was never.
func (c *probeCache) startJanitor() {
	if c.ttl <= 0 {
		return
	}
	c.stop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(max(c.ttl, time.Second))
		defer ticker.Stop()
		for {
			select {
			case <-c.stop:
				return
			case now := <-ticker.C:
				c.mu.Lock()
				for key, entry := range c.entries {
					if now.After(entry.expires) {
						c.refs -= len(entry.snapshot.Refs)
						delete(c.entries, key)
					}
				}
				c.mu.Unlock()
			}
		}
	}()
}

func (c *probeCache) stopJanitor() {
	if c.stop != nil {
		close(c.stop)
	}
}

// replay materializes the flight's outcome for a waiter.
func (f *probeFlight) replay(rt route) (protocol.Snapshot, *http.Response, error) {
	if f.err != nil {
		return protocol.Snapshot{}, nil, f.err
	}
	if f.response == nil {
		return f.snapshot, nil, nil
	}
	response := &http.Response{
		StatusCode:    f.response.status,
		Header:        f.response.header.Clone(),
		Body:          io.NopCloser(bytes.NewReader(f.response.body)),
		ContentLength: int64(len(f.response.body)),
		Request:       &http.Request{URL: rt.target(rt.repo + "/git-upload-pack")},
	}
	return protocol.Snapshot{}, response, nil
}
