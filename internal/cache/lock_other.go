//go:build !linux && !darwin && !windows

package cache

import (
	"fmt"
	"os"
)

func lockDirectory(path string) (*os.File, error) {
	return nil, fmt.Errorf("cache locking requires Linux or macOS")
}
