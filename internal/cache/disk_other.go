//go:build !unix && !windows

package cache

import "errors"

var errNoDiskUsage = errors.New("free cache space requires Linux, macOS or Windows")

func freeBytes(string) (int64, error) {
	return 0, errNoDiskUsage
}
