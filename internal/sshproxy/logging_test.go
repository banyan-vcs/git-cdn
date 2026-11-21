package sshproxy

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type summaryKey struct{}

type summaryHandler struct {
	mu      sync.Mutex
	entries []map[string]any
	ids     []int64
}

func (*summaryHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *summaryHandler) Handle(ctx context.Context, r slog.Record) error {
	entry := map[string]any{"msg": r.Message, "level": r.Level.String()}
	_, entry["has_deadline"] = ctx.Deadline()
	r.Attrs(func(a slog.Attr) bool { entry[a.Key] = a.Value.Any(); return true })
	id, _ := ctx.Value(summaryKey{}).(int64)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, entry)
	h.ids = append(h.ids, id)
	return nil
}
func (h *summaryHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *summaryHandler) WithGroup(string) slog.Handler      { return h }

func TestSSHSessionLoggingAndContexts(t *testing.T) {
	capture := &summaryHandler{}
	var connections, sessions atomic.Int64
	var mu sync.Mutex
	var infos []SessionInfo
	var deadlines []bool
	f := newSSHFixtureOptions(t, nil, []Option{
		WithLogger(slog.New(capture)),
		WithConnContext(func(ctx context.Context, _ net.Conn) context.Context {
			return context.WithValue(ctx, summaryKey{}, connections.Add(1))
		}),
		WithSessionContext(func(ctx context.Context, info SessionInfo) context.Context {
			_, deadline := ctx.Deadline()
			mu.Lock()
			infos = append(infos, info)
			deadlines = append(deadlines, deadline && ctx.Value(summaryKey{}) != nil)
			mu.Unlock()
			return context.WithValue(ctx, summaryKey{}, sessions.Add(1))
		}),
	})
	for i := range 2 {
		f.git(t, "", "clone", "--quiet", f.base, filepath.Join(t.TempDir(), "clone", string(rune('a'+i))))
	}
	f.git(t, "", "clone", "--quiet", "--depth=1", f.base, filepath.Join(t.TempDir(), "shallow"))
	f.denied.Store(true)
	u, err := url.Parse(f.base)
	require.NoError(t, err)
	check, err := knownhosts.New(f.known)
	require.NoError(t, err)
	client, err := ssh.Dial("tcp", u.Host, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.Password("fixture-token")}, HostKeyCallback: check, Timeout: 5 * time.Second})
	require.NoError(t, err)
	defer client.Close()
	session, err := client.NewSession()
	require.NoError(t, err)
	defer session.Close()
	require.NoError(t, session.Setenv("GIT_PROTOCOL", "version=2"))
	_, err = session.Output("git-upload-pack 'Group/Repo'")
	require.Error(t, err, "revoked credentials unexpectedly succeeded")

	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.entries, 4)
	for i, entry := range capture.entries {
		require.Equal(t, "SSH session completed", entry["msg"])
		require.Equal(t, int64(i+1), capture.ids[i])
		require.Equal(t, "Group/Repo.git", entry["repository"])
		require.Equal(t, "git-upload-pack", entry["command"])
		if i < 3 {
			require.EqualValues(t, 0, entry["exit_status"])
			require.Greater(t, entry["response_bytes"].(int64), int64(0))
		}
	}
	require.Equal(t, "MISS", capture.entries[0]["cache_status"])
	require.Equal(t, "HIT", capture.entries[1]["cache_status"])
	require.Equal(t, "BYPASS", capture.entries[2]["cache_status"])
	require.Equal(t, "NONE", capture.entries[3]["cache_status"])
	require.Equal(t, "WARN", capture.entries[3]["level"])
	encoded, err := json.Marshal(capture.entries)
	require.NoError(t, err)
	require.False(t, bytes.Contains(encoded, []byte("fixture-token")), "password leaked into logs")
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, infos, 4)
	for i, info := range infos {
		require.True(t, deadlines[i], "session lost connection context or request deadline")
		require.Equal(t, "git", info.User)
		require.Equal(t, "Group/Repo.git", info.Repository)
	}
}

func TestNilSSHContextCallbacksKeepContext(t *testing.T) {
	capture := &summaryHandler{}
	f := newSSHFixtureOptions(t, nil, []Option{WithLogger(slog.New(capture)), WithConnContext(func(context.Context, net.Conn) context.Context { return nil }), WithSessionContext(func(context.Context, SessionInfo) context.Context { return nil })})
	f.git(t, "", "ls-remote", f.base)
	capture.mu.Lock()
	defer capture.mu.Unlock()
	require.Len(t, capture.entries, 1)
	require.Equal(t, "SSH session completed", capture.entries[0]["msg"])
	require.True(t, capture.entries[0]["has_deadline"].(bool), "nil callback lost the request deadline")
	require.False(t, strings.Contains(capture.entries[0]["msg"].(string), "fixture-token"))
}
