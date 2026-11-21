package nativegit

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGitFailureLogsExitCodeWithoutArgumentsOrStderr(t *testing.T) {
	script := filepath.Join(t.TempDir(), "git")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' private-stderr >&2\nexit 7\n"), 0700))
	var output bytes.Buffer
	runner, err := New(script, 1, WithLogger(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	require.NoError(t, err)
	err = runner.Run(context.Background(), "", strings.NewReader("private-body"), io.Discard, map[string]string{"TOKEN": "private-environment"}, "fetch", "https://example.test/repo?token=private-url")
	require.Error(t, err)
	var event map[string]any
	require.NoError(t, json.Unmarshal(output.Bytes(), &event))
	require.Equal(t, "fetch", event["command"])
	require.EqualValues(t, 7, event["exit_code"])
	require.Equal(t, false, event["success"])
	for _, secret := range []string{"private-stderr", "private-body", "private-environment", "private-url"} {
		require.NotContains(t, output.String(), secret)
	}
}
