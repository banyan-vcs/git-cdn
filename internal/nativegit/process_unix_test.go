//go:build linux || darwin

package nativegit

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Cancellation terminates Git rather than killing it outright: its signal
// handlers delete the lock and temporary files it holds, which a killed
// process leaves in the repository.
func TestCancellationTerminatesTheProcessGroupFirst(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "git")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ntrap 'echo terminated > \"$PROBE_DIR/terminated\"; exit 0' TERM\nsleep 30 &\nwait\n"), 0700))
	runner, err := New(script, 1)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = runner.Run(ctx, "", nil, io.Discard, map[string]string{"PROBE_DIR": dir}, "fetch")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 2*time.Second, "a terminated process was waited for instead")
	require.FileExists(t, filepath.Join(dir, "terminated"), "the process group was killed without being terminated")
}

// A process that pays no attention to the termination is killed once the
// grace period in Run has passed.
func TestCancellationKillsAProcessThatIgnoresTermination(t *testing.T) {
	script := filepath.Join(t.TempDir(), "git")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nwhile :; do sleep 1; done\n"), 0700))
	runner, err := New(script, 1)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = runner.Run(ctx, "", nil, io.Discard, nil, "fetch")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Less(t, time.Since(started), 10*time.Second, "a process that ignored termination was never killed")
}
