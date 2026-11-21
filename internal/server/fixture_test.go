package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/chiyutianyi/git-cdn/internal/nativegit"
	"github.com/chiyutianyi/git-cdn/internal/protocol"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testAuthorization = "Basic dGVzdDp0b2tlbg=="

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = nativegit.Environment(map[string]string{
		"GIT_AUTHOR_NAME": "CDN Test", "GIT_AUTHOR_EMAIL": "test@example.invalid",
		"GIT_COMMITTER_NAME": "CDN Test", "GIT_COMMITTER_EMAIL": "test@example.invalid",
		"GIT_CONFIG_COUNT": "2", "GIT_CONFIG_KEY_0": "http.extraHeader", "GIT_CONFIG_VALUE_0": "Authorization: " + testAuthorization,
		"GIT_CONFIG_KEY_1": "protocol.version", "GIT_CONFIG_VALUE_1": "2",
	})
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %v\n%s", args, err, out)
	return strings.TrimSpace(string(out))
}

type fixture struct {
	work     string
	repo     string
	tip      string
	origin   *httptest.Server
	denied   atomic.Bool
	attempts atomic.Int64
	fetches  atomic.Int64
	probes   atomic.Int64
	// delay holds each authenticated origin request before it is answered,
	// widening the in-flight window that concurrent tests must overlap
	// deterministically on slow runners.
	delay atomic.Int64 // nanoseconds
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{}
	root := t.TempDir()
	f.work, f.repo = filepath.Join(root, "work"), filepath.Join(root, "Group", "Repo.git")
	testGit(t, "", "init", "--quiet", "--initial-branch=main", f.work)
	f.commit(t, "initial")
	testGit(t, f.work, "tag", "-a", "v1", "-m", "first release")
	require.NoError(t, os.MkdirAll(filepath.Dir(f.repo), 0700))
	testGit(t, "", "clone", "--quiet", "--bare", f.work, f.repo)
	testGit(t, f.repo, "config", "uploadpack.allowFilter", "true")
	f.origin = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/root/") {
			http.NotFound(w, r)
			return
		}
		f.attempts.Add(1)
		if f.denied.Load() || (r.Header.Get("Authorization") != testAuthorization && r.Header.Get("Authorization") != "Bearer second-user") {
			w.Header().Set("WWW-Authenticate", `Basic realm="Origin"`)
			w.WriteHeader(http.StatusUnauthorized)
			io.WriteString(w, "authentication required")
			return
		}
		if d := f.delay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			assert.NoError(t, err)
			http.Error(w, "read error", 500)
			return
		}
		if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/git-upload-pack") {
			decoded, err := decodeBody(body, r.Header.Get("Content-Encoding"), 8<<20)
			if err != nil {
				assert.NoError(t, err)
				http.Error(w, "encoding error", 400)
				return
			}
			body = decoded
			parsed, err := protocol.ParseFetch(body)
			if err == nil && parsed.Command == "fetch" {
				f.fetches.Add(1)
			}
			if err == nil && parsed.Command == "ls-refs" {
				f.probes.Add(1)
			}
		}
		cmd := exec.CommandContext(r.Context(), "git", "http-backend")
		cmd.Env = nativegit.Environment(map[string]string{
			"GIT_PROJECT_ROOT": root, "GIT_HTTP_EXPORT_ALL": "1",
			"PATH_INFO":      strings.TrimPrefix(r.URL.Path, "/root"),
			"REQUEST_METHOD": r.Method, "QUERY_STRING": r.URL.RawQuery,
			"CONTENT_TYPE": r.Header.Get("Content-Type"), "CONTENT_LENGTH": strconv.Itoa(len(body)),
			"SERVER_PROTOCOL": "HTTP/1.1", "REMOTE_USER": "test",
			"HTTP_GIT_PROTOCOL": r.Header.Get("Git-Protocol"),
		})
		cmd.Stdin = bytes.NewReader(body)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			assert.Fail(t, "test failed", "http-backend: %v: %s", err, stderr.String())
			http.Error(w, "backend failure", 500)
			return
		}
		reader := bufio.NewReader(&out)
		headers, err := textproto.NewReader(reader).ReadMIMEHeader()
		if err != nil {
			assert.NoError(t, err)
			http.Error(w, "bad CGI headers", 500)
			return
		}
		status := 200
		if h := headers.Get("Status"); h != "" {
			status, _ = strconv.Atoi(strings.Fields(h)[0])
			headers.Del("Status")
		}
		for name, values := range headers {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		w.WriteHeader(status)
		io.Copy(w, reader)
	}))
	t.Cleanup(f.origin.Close)
	return f
}

func (f *fixture) commit(t *testing.T, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "file.txt"), []byte(content+"\n"), 0600))
	testGit(t, f.work, "add", "file.txt")
	testGit(t, f.work, "commit", "--quiet", "-m", content)
	f.tip = testGit(t, f.work, "rev-parse", "HEAD")
}

func newCDN(t *testing.T, f *fixture, modify func(*config.Config)) (*Server, *httptest.Server) {
	t.Helper()
	c := config.Config{CacheDir: t.TempDir(), AllowSharedCredentialHost: true, Upstreams: map[string]config.Upstream{"gitlab": {URL: f.origin.URL + "/root"}}}
	if modify != nil {
		modify(&c)
	}
	s, err := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	h := httptest.NewServer(s)
	t.Cleanup(func() { h.Close(); s.Close() })
	return s, h
}

func fetchBody(oid string, args ...string) []byte {
	var b bytes.Buffer
	b.Write(protocol.Encode("command=fetch\n"))
	b.WriteString("0001")
	b.Write(protocol.Encode("want " + oid + "\n"))
	for _, arg := range args {
		b.Write(protocol.Encode(arg + "\n"))
	}
	b.Write(protocol.Encode("done\n"))
	b.WriteString("0000")
	return b.Bytes()
}

func postFetch(t *testing.T, base, path string, body []byte, authorization string, extras http.Header) *http.Response {
	t.Helper()
	r, err := http.NewRequest("POST", base+path, bytes.NewReader(body))
	require.NoError(t, err)
	r.Header.Set("Authorization", authorization)
	r.Header.Set("Git-Protocol", "version=2")
	r.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	for name, values := range extras {
		r.Header[name] = values
	}
	response, err := http.DefaultClient.Do(r)
	require.NoError(t, err)
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func responseBody(t *testing.T, r *http.Response) []byte {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	require.NoError(t, err)
	r.Body.Close()
	require.EqualValues(t, 200, r.StatusCode, "status %d: %s", r.StatusCode, body)
	return body
}

func newYAMLCDN(t *testing.T, body string) (*Server, *httptest.Server) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	body += fmt.Sprintf("\nallow_shared_credential_host: true\ncache_dir: %q\n", t.TempDir())
	require.NoError(t, os.WriteFile(path, []byte(body), 0600))
	c, err := config.Load(path)
	require.NoError(t, err)
	s, err := New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	h := httptest.NewServer(s)
	t.Cleanup(func() { h.Close(); s.Close() })
	return s, h
}
