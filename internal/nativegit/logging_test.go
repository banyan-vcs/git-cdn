package nativegit

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
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

// A failure reaches the log with its class, so a lock someone else holds
// can be told from missing objects and from a transfer the origin broke
// off. Git's stderr is what the class is read from, and still does not
// reach the log. The fake git is a shell script, like the one above.
func TestGitFailuresAreClassifiedWithoutLoggingStderr(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake git is a /bin/sh script")
	}
	for _, test := range []struct {
		name, stderr string
		kind         ErrorKind
	}{
		{"reftable lock", "fatal: update_ref failed for ref 'refs/heads/main': cannot lock references\n", FailureRefLocked},
		{"files lock", "fatal: cannot lock ref 'refs/heads/main': Unable to create '/private-path/main.lock': File exists.\n", FailureRefLocked},
		{"missing objects", "fatal: bad object private-object\n", FailureObjectsMissing},
		{"origin", "fatal: unable to access 'https://example.test/private-token/repo/': Failed to connect to example.test port 443\n", FailureOrigin},
		{"unclassified", "private-stderr\n", FailureOther},
	} {
		t.Run(test.name, func(t *testing.T) {
			script := filepath.Join(t.TempDir(), "git")
			require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$PROBE_STDERR\" >&2\nexit 128\n"), 0700))
			var output bytes.Buffer
			runner, err := New(script, 1, WithLogger(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug}))))
			require.NoError(t, err)
			err = runner.Run(context.Background(), "", nil, io.Discard, map[string]string{"PROBE_STDERR": test.stderr}, "update-ref")
			require.Error(t, err)
			var failure *Error
			require.ErrorAs(t, err, &failure)
			require.Equal(t, test.kind, failure.Kind)
			var event map[string]any
			require.NoError(t, json.Unmarshal(output.Bytes(), &event))
			require.Equal(t, string(test.kind), event["error_reason"])
			require.EqualValues(t, 128, event["exit_code"])
			require.NotContains(t, output.String(), "private-")
		})
	}
}
