package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
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
	s, h := newCDN(t, f, nil)
	var output eventBuffer
	s.logger = slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))
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
	for _, message := range []string{"upstream refs refreshed", "cache objects hydrated", "cache objects reused", "local fetch prepared", "upstream passthrough completed"} {
		require.Contains(t, messages, message)
	}
	require.NotContains(t, output.String(), testAuthorization)
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
