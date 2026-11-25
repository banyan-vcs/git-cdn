package nativegit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
)

type Runner struct {
	Binary string
	slots  chan struct{}
	logger *slog.Logger
}

type Option func(*Runner)

func WithLogger(logger *slog.Logger) Option {
	return func(r *Runner) { r.logger = logger }
}

func (r *Runner) Logger() *slog.Logger {
	if r == nil || r.logger == nil {
		return slog.Default()
	}
	return r.logger
}

func New(binary string, concurrency int, options ...Option) (*Runner, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	r := &Runner{Binary: path, slots: make(chan struct{}, concurrency)}
	for _, option := range options {
		option(r)
	}
	return r, nil
}

// baseEnvironment filters the process environment once; every spawned git
// inherits the same stable prefix, so a process-heavy request path does not
// re-walk os.Environ per invocation.
var (
	baseEnvOnce sync.Once
	baseEnv     []string
)

func baseEnvironment() []string {
	baseEnvOnce.Do(func() {
		for _, v := range os.Environ() {
			name, _, _ := strings.Cut(v, "=")
			if strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "SSH_") || name == "LANG" || name == "LC_ALL" || strings.EqualFold(name, "http_proxy") || strings.EqualFold(name, "https_proxy") || strings.EqualFold(name, "all_proxy") || strings.EqualFold(name, "no_proxy") {
				continue
			}
			baseEnv = append(baseEnv, v)
		}
	})
	return baseEnv
}

func Environment(extra map[string]string) []string {
	env := append([]string{}, baseEnvironment()...)
	defaults := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1",
		"GIT_ATTR_NOSYSTEM": "1", "LANG": "C", "LC_ALL": "C", "NO_PROXY": "*",
		// Internal ref transactions must not derive an email through hostname/DNS lookup.
		"GIT_COMMITTER_NAME": "Git CDN", "GIT_COMMITTER_EMAIL": "git-cdn@example.invalid",
	}
	for k, v := range extra {
		defaults[k] = v
	}
	for k, v := range defaults {
		env = append(env, k+"="+v)
	}
	return env
}

func ConfigEnvironment(pairs [][2]string) map[string]string {
	env := map[string]string{"GIT_CONFIG_COUNT": strconv.Itoa(len(pairs))}
	for i, pair := range pairs {
		env["GIT_CONFIG_KEY_"+strconv.Itoa(i)] = pair[0]
		env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)] = pair[1]
	}
	return env
}

type cancellingWriter struct {
	w      io.Writer
	cancel context.CancelFunc
	err    error
}

func (w *cancellingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		w.err = err
		w.cancel()
	}
	return n, err
}

func (r *Runner) Run(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, env map[string]string, args ...string) (err error) {
	started := time.Now()
	var queueTime time.Duration
	defer func() {
		attrs := []slog.Attr{slog.String("command", commandName(args)), slog.Int64("duration_ms", time.Since(started).Milliseconds()),
			slog.Int64("queue_wait_ms", queueTime.Milliseconds()), slog.Bool("success", err == nil)}
		attrs = append(attrs, logging.ErrorAttrs(err)...)
		logging.Logger(ctx, r.Logger()).LogAttrs(ctx, slog.LevelDebug, "native Git command completed", attrs...)
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case r.slots <- struct{}{}:
		queueTime = time.Since(started)
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(childCtx, r.Binary, args...)
	output := &cancellingWriter{w: stdout, cancel: cancel}
	cmd.Dir, cmd.Stdin, cmd.Stdout = dir, stdin, output
	cmd.Env = Environment(env)
	cmd.Stderr = io.Discard
	configureProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if output.err != nil {
			return fmt.Errorf("git output failed: %w", output.err)
		}
		// Git's stderr can contain upstream-provided text or authentication data.
		return fmt.Errorf("git operation failed: %w", err)
	}
	return nil
}

func commandName(args []string) string {
	for _, arg := range args {
		switch arg {
		case "init", "fetch", "upload-pack", "receive-pack", "index-pack", "rev-list", "update-ref", "for-each-ref", "cat-file", "merge-base", "repack", "prune", "commit-graph", "symbolic-ref", "rev-parse":
			return arg
		}
	}
	return "other"
}

// MetadataOutputLimit bounds what one metadata command may return in
// memory. Tests lower it to exercise callers' handling of oversized output.
var MetadataOutputLimit = 16 << 20

func (r *Runner) Output(ctx context.Context, dir string, input string, args ...string) ([]byte, error) {
	var b bytes.Buffer
	err := r.Run(ctx, dir, strings.NewReader(input), &boundedOutput{buffer: &b, left: MetadataOutputLimit}, nil, args...)
	return b.Bytes(), err
}

// ErrOutputLimit is returned when a metadata command prints more than the
// in-memory output bound. Callers that ask for an open-ended listing use it
// to tell an oversized answer from a failed command.
var ErrOutputLimit = errors.New("git metadata output exceeds limit")

type boundedOutput struct {
	buffer *bytes.Buffer
	left   int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, ErrOutputLimit
	}
	w.left -= len(p)
	return w.buffer.Write(p)
}
