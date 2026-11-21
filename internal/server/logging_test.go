package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"github.com/stretchr/testify/require"
)

type requestLogKey struct{}
type requestLogHandler struct {
	slog.Handler
	seen func(context.Context)
}

func (h requestLogHandler) Handle(ctx context.Context, r slog.Record) error {
	h.seen(ctx)
	return h.Handler.Handle(ctx, r)
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
