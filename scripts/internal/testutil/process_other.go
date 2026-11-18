//go:build !unix

package testutil

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
