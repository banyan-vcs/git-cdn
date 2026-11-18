// Package testutil controls native Git processes used by the validation tools.
package testutil

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

func Command(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 2 * time.Second
	configureProcess(cmd)
	return cmd
}

// Environment excludes caller Git configuration and credential helpers.
func Environment() []string {
	var env []string
	for _, value := range os.Environ() {
		if !strings.HasPrefix(value, "GIT_") && !strings.HasPrefix(value, "SSH_") {
			env = append(env, value)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0", "GIT_TRACE_REDACT=1", "GIT_NO_LAZY_FETCH=1",
		"GIT_AUTHOR_NAME=Git CDN Probe", "GIT_AUTHOR_EMAIL=probe@example.invalid",
		"GIT_COMMITTER_NAME=Git CDN Probe", "GIT_COMMITTER_EMAIL=probe@example.invalid",
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00+00:00", "GIT_COMMITTER_DATE=2026-01-01T00:00:00+00:00",
		"NO_PROXY=127.0.0.1,localhost", "LANG=C", "LC_ALL=C")
}
