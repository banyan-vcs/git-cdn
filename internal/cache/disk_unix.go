//go:build unix

package cache

import "syscall"

func freeBytes(path string) (int64, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, err
	}
	// The blocks available outside the filesystem's reserved share: what a
	// cache that does not run as the superuser can grow into.
	return int64(stat.Bsize) * int64(stat.Bavail), nil
}
