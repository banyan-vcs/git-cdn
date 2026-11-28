//go:build windows

package cache

import "golang.org/x/sys/windows"

func freeBytes(path string) (int64, error) {
	dir, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var available, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(dir, &available, &total, &free); err != nil {
		return 0, err
	}
	return int64(available), nil
}
