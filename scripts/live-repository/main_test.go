package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/server"
	"gitlab.com/banyan-vcs/git-cdn/scripts/internal/testutil"

	"github.com/stretchr/testify/require"
)

// TestLiveSuiteOffline exercises the complete harness using a local Git origin,
// including child CDN processes, without depending on external network state.
func TestLiveSuiteOffline(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("native Git is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	root := t.TempDir()
	fixtureGit := func(dir string, args ...string) {
		t.Helper()
		cmd := testutil.Command(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, testutil.Environment()
		{
			out, err := cmd.CombinedOutput()
			require.NoError(t, err, "git %v: %v: %s", args, err, out)
		}
	}
	work := filepath.Join(root, "work")
	fixtureGit("", "init", "-q", "--initial-branch=main", "--object-format=sha1", work)
	for index := 0; index < 20; index++ {
		require.NoError(t, os.WriteFile(filepath.Join(work, "README.md"), []byte(fmt.Sprintf("revision %d\n", index)), 0600))
		fixtureGit(work, "add", "README.md")
		fixtureGit(work, "commit", "-q", "-m", fmt.Sprintf("revision %d", index))
	}
	originRoot := filepath.Join(root, "origin")
	repo := filepath.Join(originRoot, "example", "project.git")
	require.NoError(t, os.MkdirAll(filepath.Dir(repo), 0700))
	fixtureGit("", "clone", "-q", "--bare", "--no-local", work, repo)
	fixtureGit(repo, "config", "uploadpack.allowFilter", "true")
	fixtureGit(repo, "config", "uploadpack.allowAnySHA1InWant", "true")
	gitPath, _ := exec.LookPath("git")
	origin := httptest.NewServer(&cgi.Handler{Path: gitPath, Args: []string{"http-backend"}, Dir: originRoot,
		Env: append(testutil.Environment(), "GIT_PROJECT_ROOT="+originRoot, "GIT_HTTP_EXPORT_ALL=1")})
	defer origin.Close()
	handler, err := server.New(config.Config{CacheDir: filepath.Join(root, "cache"),
		Upstreams: map[string]config.Upstream{"github": {URL: origin.URL}}}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	defer handler.Close()
	cdn := httptest.NewServer(handler)
	defer cdn.Close()
	binary := filepath.Join(root, "git-cdn")
	build := testutil.Command(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/git-cdn")
	build.Dir = "../.."
	{
		out, err := build.CombinedOutput()
		require.NoError(t, err, "build test server: %v: %s", err, out)
	}
	output := filepath.Join(root, "results")
	require.NoError(t, os.MkdirAll(filepath.Join(output, "logs"), 0700))
	s := &suite{args: options{base: cdn.URL, origin: origin.URL, repository: "example/project", binary: binary, output: output, concurrency: 6, timeout: 30},
		url: cdn.URL + "/github/example/project", ctx: ctx, client: &http.Client{Timeout: 10 * time.Second}, report: map[string]any{}}
	details, failure := capture(func() map[string]any { return map[string]any{"passed": s.run()} })
	if failure != nil || details["passed"] != true {
		data, _ := os.ReadFile(filepath.Join(output, "results.json"))
		// Command traces are retained in the report; case results keep failures readable.
		var cases bytes.Buffer
		for _, item := range s.cases {
			fmt.Fprintln(&cases, item)
		}
		require.FailNow(t, "test failed", "offline suite: %v\n%s\nreport bytes: %d", failure, cases.String(), len(data))
	}
}
