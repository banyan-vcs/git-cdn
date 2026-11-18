package nativegit

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Runner struct {
	Binary string
	slots  chan struct{}
}

func New(binary string, concurrency int) (*Runner, error) {
	path, err := exec.LookPath(binary)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	return &Runner{Binary: path, slots: make(chan struct{}, concurrency)}, nil
}

func Environment(extra map[string]string) []string {
	var env []string
	for _, v := range os.Environ() {
		name, _, _ := strings.Cut(v, "=")
		if strings.HasPrefix(name, "GIT_") || strings.HasPrefix(name, "SSH_") || name == "LANG" || name == "LC_ALL" || strings.EqualFold(name, "http_proxy") || strings.EqualFold(name, "https_proxy") || strings.EqualFold(name, "all_proxy") || strings.EqualFold(name, "no_proxy") {
			continue
		}
		env = append(env, v)
	}
	defaults := map[string]string{
		"GIT_CONFIG_NOSYSTEM": "1", "GIT_CONFIG_GLOBAL": os.DevNull,
		"GIT_TERMINAL_PROMPT": "0", "GIT_NO_LAZY_FETCH": "1",
		"GIT_ATTR_NOSYSTEM": "1", "LANG": "C", "LC_ALL": "C", "NO_PROXY": "*",
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
}

func (w cancellingWriter) Write(p []byte) (int, error) {
	n, err := w.w.Write(p)
	if err != nil {
		w.cancel()
	}
	return n, err
}

func (r *Runner) Run(ctx context.Context, dir string, stdin io.Reader, stdout io.Writer, env map[string]string, args ...string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case r.slots <- struct{}{}:
		defer func() { <-r.slots }()
	case <-ctx.Done():
		return ctx.Err()
	}
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(childCtx, r.Binary, args...)
	cmd.Dir, cmd.Stdin, cmd.Stdout = dir, stdin, cancellingWriter{w: stdout, cancel: cancel}
	cmd.Env = Environment(env)
	cmd.Stderr = io.Discard
	configureProcess(cmd)
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Git's stderr can contain upstream-provided text or authentication data.
		return fmt.Errorf("git operation failed: %w", err)
	}
	return nil
}

func (r *Runner) Output(ctx context.Context, dir string, input string, args ...string) ([]byte, error) {
	var b bytes.Buffer
	err := r.Run(ctx, dir, strings.NewReader(input), &boundedOutput{buffer: &b, left: 16 << 20}, nil, args...)
	return b.Bytes(), err
}

type boundedOutput struct {
	buffer *bytes.Buffer
	left   int
}

func (w *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > w.left {
		return 0, fmt.Errorf("git metadata output exceeds limit")
	}
	w.left -= len(p)
	return w.buffer.Write(p)
}
