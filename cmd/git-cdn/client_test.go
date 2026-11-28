package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeAdmin implements just enough of the operations endpoint to exercise
// the commands: a status document, the scope list, drain state, and a
// reload report. busy counts down with each status read, which drives the
// --wait polling.
func fakeAdmin(t *testing.T) *httptest.Server {
	t.Helper()
	var draining atomic.Bool
	var busy atomic.Int32
	busy.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		active := 0
		if current := busy.Load(); current > 0 {
			active = int(current)
			busy.Add(-1)
		}
		status := "ok"
		if draining.Load() {
			status = "draining"
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"` + status + `","version":"test","mode":"single","listen":"127.0.0.1:8080",` +
			`"uptime_seconds":3660,"active_requests":` + itoa(active) + `,"active_ssh_sessions":0,` +
			`"cache":{"entries":2,"bytes":1048576,"max_bytes":21474836480,"reserved_bytes":0},` +
			`"disk_free_bytes":1099511627776,` +
			`"totals":{"local_requests":5,"passthrough_requests":2,"hydrated_repositories":1,"rejected_requests":0,"pack_cache_hits":3}}`))
	})
	mux.HandleFunc("GET /scopes", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"scopes":[{"scope":"5f3aa19b21c0ffe5a4b8c1d0e2f3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e0f1a2b3",` +
			`"site":"gitlab","bytes":5242880,"pack_bytes":1048576,"packs":2,"requests":7,` +
			`"last_access":"2026-11-28T05:00:00Z"}]}`))
	})
	mux.HandleFunc("POST /drain", func(w http.ResponseWriter, r *http.Request) {
		draining.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"draining","active_requests":1,"active_ssh_sessions":0}`))
	})
	mux.HandleFunc("POST /undrain", func(w http.ResponseWriter, r *http.Request) {
		draining.Store(false)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok","active_requests":0,"active_ssh_sessions":0}`))
	})
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"applied":[{"field":"max_cache_bytes","old":"10737418240","new":"5368709120"}],` +
			`"restart_required":[{"field":"listen","old":"127.0.0.1:8080","new":"127.0.0.1:9090"}]}`))
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out strings.Builder
	root := newRootCommand()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestStatusCommandPrintsSummary(t *testing.T) {
	server := fakeAdmin(t)
	out, err := runCommand(t, "status", "--addr", server.URL)
	require.NoError(t, err)
	require.Contains(t, out, "status    ok (version test, uptime 61m)")
	require.Contains(t, out, "cache     2 repositories, 1.0 MiB of 20.0 GiB (0.0%)")
	require.Contains(t, out, "1.0 TiB free")
	require.Contains(t, out, "active    1 requests, 0 ssh sessions")
	require.Contains(t, out, "traffic   5 local, 2 passthrough, 1 hydrations, 3 pack hits, 0 rejected")
}

func TestScopesCommandShowsHashNotName(t *testing.T) {
	server := fakeAdmin(t)
	out, err := runCommand(t, "scopes", "--addr", server.URL)
	require.NoError(t, err)
	require.Contains(t, out, "5f3aa19b21c0…")
	require.Contains(t, out, "gitlab")
	require.Contains(t, out, "5.0 MiB")
	require.NotContains(t, out, "repo", "a repository name leaked into the scope view")
}

func TestDrainWaitPollsUntilQuiet(t *testing.T) {
	server := fakeAdmin(t)
	// The fake's status reports one active request on the first read and
	// zero afterwards, so --wait must poll instead of returning at once.
	out, err := runCommand(t, "drain", "--addr", server.URL, "--wait", "--wait-timeout", "5s")
	require.NoError(t, err)
	require.Contains(t, out, "draining: 1 requests, 0 ssh sessions in flight")
	require.Contains(t, out, "node is quiet")
}

func TestDrainWaitReportsWhatRemains(t *testing.T) {
	// A node whose work never ends: status always reports one request.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/drain" {
			w.Write([]byte(`{"status":"draining","active_requests":1,"active_ssh_sessions":0}`))
			return
		}
		w.Write([]byte(`{"status":"draining","active_requests":1,"active_ssh_sessions":0,"cache":{},"totals":{}}`))
	}))
	t.Cleanup(server.Close)
	_, err := runCommand(t, "drain", "--addr", server.URL, "--wait", "--wait-timeout", "10ms")
	require.ErrorContains(t, err, "still draining")
}

func TestUndrainCommand(t *testing.T) {
	server := fakeAdmin(t)
	out, err := runCommand(t, "undrain", "--addr", server.URL)
	require.NoError(t, err)
	require.Contains(t, out, "ok: 0 requests, 0 ssh sessions in flight")
}

func TestReloadCommandPrintsAppliedAndRestart(t *testing.T) {
	server := fakeAdmin(t)
	out, err := runCommand(t, "reload", "--addr", server.URL)
	require.NoError(t, err)
	require.Contains(t, out, "applied   max_cache_bytes: 10737418240 -> 5368709120")
	require.Contains(t, out, "restart   listen: 127.0.0.1:8080 -> 127.0.0.1:9090")
}

func TestJSONFlagReturnsRawDocuments(t *testing.T) {
	server := fakeAdmin(t)
	out, err := runCommand(t, "status", "--addr", server.URL, "--json")
	require.NoError(t, err)
	require.Contains(t, out, `"version":"test"`)
	require.Contains(t, out, `"disk_free_bytes":1099511627776`)
}

func TestUnreachableEndpointIsAPlainError(t *testing.T) {
	// Port 1 on loopback refuses connections without a listener.
	_, err := runCommand(t, "status", "--addr", "http://127.0.0.1:1", "--timeout", "500ms")
	require.Error(t, err)
}

// The wait loop must respect cancellation, not just its deadline.
func TestFormatHelpers(t *testing.T) {
	require.Equal(t, "512.0 MiB", formatBytes(512<<20))
	require.Equal(t, "1.5 GiB", formatBytes(3<<30/2))
	require.Equal(t, "45s", formatUptime(45))
	require.Equal(t, "72m", formatUptime(4320))
	require.Equal(t, "2.0h", formatUptime(7200))
	require.Equal(t, "now", formatAge(-time.Second))
	require.Equal(t, "3m ago", formatAge(3*time.Minute))
	require.Equal(t, "2.0d ago", formatAge(48*time.Hour))
}
