package nativegit

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRequireReftableChecksNativeGitCapability(t *testing.T) {
	runner, err := New("git", 1)
	require.NoError(t, err)
	require.NoError(t, runner.RequireReftable(context.Background()))
	oldGit := filepath.Join(t.TempDir(), "unsupported-git")
	require.NoError(t, os.WriteFile(oldGit, []byte("#!/bin/sh\nexit 129\n"), 0700))
	runner, err = New(oldGit, 1)
	require.NoError(t, err)
	require.ErrorContains(t, runner.RequireReftable(context.Background()), "native Git must support reftable (Git 2.45+)")
}
