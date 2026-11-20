//go:build windows

package cache

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockDirectory takes an exclusive, non-blocking lock on one byte of the
// lock file, matching the flock-based unix behavior: the lock is released
// when the descriptor closes, including on process death.
func lockDirectory(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{}); err != nil {
		f.Close()
		return nil, fmt.Errorf("cache directory is already in use: %w", err)
	}
	return f, nil
}
