package server

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

type requestLogKey struct{}
type requestLogHandler struct {
	slog.Handler
	seen func(context.Context)
}

type eventBuffer struct {
	sync.Mutex
	bytes.Buffer
}

func (b *eventBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.Buffer.Write(p)
}

func TestRepositoryLogsTraceHydrationReuseAndBypass(t *testing.T) {
	f := newFixture(t)
	var output eventBuffer
	_, h := newCDNWithLogger(t, f, nil, slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for i, body := range [][]byte{fetchBody(f.tip), fetchBody(f.tip), fetchBody(f.tip, "filter blob:none")} {
		path := "/gitlab/Group/Repo.git/git-upload-pack"
		if i == 2 {
			path += "?logging=passthrough"
		}
		r := postFetch(t, h.URL, path, body, testAuthorization, nil)
		responseBody(t, r)
		if i == 2 {
			require.Equal(t, "BYPASS", r.Header.Get("X-GitCDN-Cache-Status"))
		}
	}
	output.Lock()
	defer output.Unlock()
	var summaries []map[string]any
	var messages []string
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var event map[string]any
		require.NoError(t, decoder.Decode(&event))
		messages = append(messages, event["msg"].(string))
		if event["msg"] == "repository request completed" {
			require.Equal(t, "Group/Repo.git", event["repository"])
			require.Equal(t, "gitlab", event["upstream"])
			require.Greater(t, event["response_bytes"].(float64), float64(0))
			require.NotContains(t, event, "error_kind", "successful request was logged as canceled")
			summaries = append(summaries, event)
		}
	}
	require.Len(t, summaries, 3)
	require.Equal(t, "MISS", summaries[0]["cache_status"])
	require.Equal(t, "HIT", summaries[1]["cache_status"])
	require.Equal(t, "BYPASS", summaries[2]["cache_status"])
	require.Equal(t, "MISS", summaries[0]["pack_cache_status"])
	require.Equal(t, "HIT", summaries[1]["pack_cache_status"])
	require.Equal(t, "NONE", summaries[2]["pack_cache_status"])
	require.Equal(t, "LIVE", summaries[1]["auth_cache_status"])
	for _, message := range []string{"upstream refs refreshed", "cache objects hydrated", "cache objects reused", "local fetch prepared", "upstream passthrough completed", "local pack generated", "response pack stream selected", "authorization probe completed"} {
		require.Contains(t, messages, message)
	}
	require.NotContains(t, output.String(), testAuthorization)
}

func TestAuthorizationProbeLogsLifetimeReuse(t *testing.T) {
	f := newFixture(t)
	var output eventBuffer
	_, h := newCDNWithLogger(t, f, func(c *config.Config) { c.AuthProbeCacheTTL = "10s" }, slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
	for range 2 {
		responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
	}
	output.Lock()
	defer output.Unlock()
	var probes, summaries []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(output.Bytes()))
	for decoder.More() {
		var event map[string]any
		require.NoError(t, decoder.Decode(&event))
		switch event["msg"] {
		case "authorization probe completed":
			probes = append(probes, event)
		case "repository request completed":
			summaries = append(summaries, event)
		}
	}
	require.Len(t, probes, 2)
	require.Equal(t, "LIVE", probes[0]["probe_result"])
	require.Equal(t, "HIT", probes[1]["probe_result"])
	require.EqualValues(t, 10000, probes[1]["auth_cache_ttl_ms"])
	require.Len(t, summaries, 2)
	require.Equal(t, "HIT", summaries[1]["auth_cache_status"])
	require.NotContains(t, output.String(), testAuthorization)
}

func TestDetachedProbeRetainsLoggerAndCorrelation(t *testing.T) {
	f := newFixture(t)
	s, _ := newCDN(t, f, nil)
	var active atomic.Int64
	var output eventBuffer
	logger := slog.New(requestLogHandler{Handler: slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}), seen: func(ctx context.Context) {
		require.Equal(t, "leader-42", ctx.Value(requestLogKey{}))
		if ctx.Err() == nil {
			active.Add(1)
		}
	}}).With("request_id", "leader-42")
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), requestLogKey{}, "leader-42"))
	cancel()
	r := httptest.NewRequest(http.MethodPost, "https://cdn.example/gitlab/Group/Repo.git/git-upload-pack", nil)
	r.Header.Set("Authorization", testAuthorization)
	r = r.WithContext(logging.WithLogger(ctx, logger))
	rt, err := s.resolve(r)
	require.NoError(t, err)
	snapshot, response, cached, err := s.probe(r, rt)
	require.NoError(t, err)
	require.Nil(t, response)
	require.False(t, cached)
	require.NotEmpty(t, snapshot.Refs)
	require.Positive(t, active.Load(), "detached upstream logger lost the live probe context")
	require.Contains(t, output.String(), "upstream refs refreshed")
	require.Contains(t, output.String(), `"request_id":"leader-42"`)
	require.NotContains(t, output.String(), testAuthorization)
}

func TestResponseGenerationWaitLogsCompletionAndCancellation(t *testing.T) {
	for _, canceled := range []bool{false, true} {
		name := "completed"
		if canceled {
			name = "canceled"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			var output eventBuffer
			s, h := newCDNWithLogger(t, f, nil, slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug})))
			responseBody(t, postFetch(t, h.URL, "/gitlab/Group/Repo.git/git-upload-pack", fetchBody(f.tip), testAuthorization, nil))
			r := httptest.NewRequest(http.MethodPost, "https://cdn.example/gitlab/Group/Repo.git/git-upload-pack", nil)
			r.Header.Set("Authorization", testAuthorization)
			rt, err := s.resolve(r)
			require.NoError(t, err)
			snapshot, response, _, err := s.probe(r, rt)
			require.NoError(t, err)
			require.Nil(t, response)
			data := fetchBody(f.tip, "deepen 1")
			fetch, err := protocol.ParseFetch(data)
			require.NoError(t, err)
			identity := scopeIdentity(rt.base, rt.repo)
			key := s.packCacheKey(identity, fetch, data, snapshot)
			_, lead := s.flights.waitOrLead(s.store.Key(identity) + "/" + hex.EncodeToString(key))
			require.NotNil(t, lead)
			var release sync.Once
			defer release.Do(lead)
			ctx, cancel := context.WithCancel(r.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				file, _, _, err := s.prepare(r.WithContext(ctx), rt, fetch, snapshot, data)
				if file != nil {
					file.Close()
				}
				done <- err
			}()
			require.Eventually(t, func() bool { return s.flightWaits.Load() > 0 }, 5*time.Second, 10*time.Millisecond)
			if canceled {
				cancel()
			} else {
				release.Do(lead)
			}
			select {
			case err := <-done:
				if canceled {
					require.ErrorIs(t, err, context.Canceled)
				} else {
					require.NoError(t, err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("generation waiter did not finish")
			}
			output.Lock()
			defer output.Unlock()
			require.Contains(t, output.String(), "response pack generation wait "+name)
			require.NotContains(t, output.String(), testAuthorization)
		})
	}
}

func (h requestLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.seen(ctx)
	return h.Handler.Handle(ctx, r)
}

func (h requestLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return requestLogHandler{Handler: h.Handler.WithAttrs(attrs), seen: h.seen}
}

func (h requestLogHandler) WithGroup(name string) slog.Handler {
	return requestLogHandler{Handler: h.Handler.WithGroup(name), seen: h.seen}
}

func TestRequestErrorLogsRetainContext(t *testing.T) {
	origin := httptest.NewTLSServer(http.NotFoundHandler())
	origin.Close()
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		t.Run(method, func(t *testing.T) {
			var seen context.Context
			logger := slog.New(requestLogHandler{Handler: slog.NewTextHandler(io.Discard, nil), seen: func(ctx context.Context) { seen = ctx }})
			s, err := New(config.Config{CacheDir: t.TempDir(), Upstream: &config.Upstream{URL: origin.URL}}, logger)
			require.NoError(t, err)
			defer s.Close()
			path := "/Group/Repo.git/info/refs?service=git-upload-pack"
			if method == http.MethodPost {
				path = "/Group/Repo.git/git-upload-pack"
			}
			r := httptest.NewRequest(method, "https://cdn.example"+path, strings.NewReader(string(fetchBody(strings.Repeat("1", 40)))))
			r.Header.Set("Git-Protocol", "version=2")
			r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
			r = r.WithContext(context.WithValue(r.Context(), requestLogKey{}, "request-42"))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			require.Equal(t, http.StatusBadGateway, w.Code)
			require.NotNil(t, seen)
			require.Equal(t, "request-42", seen.Value(requestLogKey{}))
		})
	}
}
