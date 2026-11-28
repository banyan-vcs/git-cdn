//go:build linux || darwin

package nativegit

import (
	"os"
	"os/exec"
	"syscall"
)

func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Git deletes the lock and temporary files it holds before it dies from
	// a caught signal, so the process group is terminated rather than
	// killed outright. A process that has not exited when Run's WaitDelay
	// passes is killed after all.
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		if err == syscall.ESRCH {
			return os.ErrProcessDone
		}
		return err
	}
}
