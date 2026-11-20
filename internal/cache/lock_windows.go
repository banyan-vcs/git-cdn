//go:build windows

package cache

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// lockDirectory takes an exclusive, non-blocking range lock far past the
// end of the (empty) lock file, matching the flock-based unix behavior:
// mutual exclusion between processes without interfering with readers of
// the file itself, released when the descriptor closes, including on
// process death.
func lockDirectory(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	offset := int64(1) << 30
	overlapped := windows.Overlapped{Offset: uint32(offset), OffsetHigh: uint32(offset >> 32)}
	if err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &overlapped); err != nil {
		f.Close()
		return nil, fmt.Errorf("cache directory is already in use: %w", err)
	}
	return f, nil
}
