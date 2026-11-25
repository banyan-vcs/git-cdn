package nativegit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// InitBare creates an empty reftable repository. The template is emptied:
// every hydration staging directory and every per-request view would
// otherwise receive the sample hooks and description files, which are
// created and deleted thousands of times without ever being read.
func (r *Runner) InitBare(ctx context.Context, dir string) error {
	return r.Run(ctx, "", nil, io.Discard, nil, "init", "--quiet", "--bare", "--template=", "--object-format=sha1", "--ref-format=reftable", dir)
}

func (r *Runner) RequireReftable(ctx context.Context) error {
	dir, err := os.MkdirTemp("", "git-cdn-reftable-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := r.InitBare(ctx, dir); err != nil {
		return fmt.Errorf("native Git must support reftable (Git 2.45+): %w", err)
	}
	return nil
}

// HEAD may be stored in reftable rather than in the compatibility HEAD file.
func (r *Runner) Head(ctx context.Context, dir string) (string, error) {
	out, err := r.Output(ctx, dir, "", "symbolic-ref", "--quiet", "HEAD")
	if err == nil {
		return "ref: " + strings.TrimSpace(string(out)) + "\n", nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 {
		return "", err
	}
	out, err = r.Output(ctx, dir, "", "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)) + "\n", nil
}

func (r *Runner) SetHead(ctx context.Context, dir, head string) error {
	if strings.HasPrefix(head, "ref: ") {
		return r.Run(ctx, dir, nil, io.Discard, nil, "symbolic-ref", "HEAD", strings.TrimSpace(strings.TrimPrefix(head, "ref: ")))
	}
	return r.Run(ctx, dir, nil, io.Discard, nil, "update-ref", "--no-deref", "HEAD", strings.TrimSpace(head))
}
