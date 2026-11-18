//go:build !linux && !darwin

package nativegit

import "os/exec"

func configureProcess(cmd *exec.Cmd) {}
