//go:build linux || darwin

package cache

import (
	"fmt"
	"os"
	"syscall"
)

func lockDirectory(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("cache directory is already in use: %w", err)
	}
	return f, nil
}
