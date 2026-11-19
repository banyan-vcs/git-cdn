package cache

import (
	"os"
	"path/filepath"
)

// Reftable transactions create new tables and may compact the existing stack.
// Reserve space for the stack rewrite, uncompressed updates and block/index
// overhead. Files repositories retain the packed-refs rewrite reservation.
func refUpdateBudget(dir string, updates int) (int64, error) {
	records := 2 * int64(updates)
	path := filepath.Join(dir, "reftable")
	if info, err := os.Stat(path); err == nil && info.IsDir() {
		size, err := directorySize(path)
		return 2*size + records + 1024, err
	} else if err != nil && !os.IsNotExist(err) {
		return 0, err
	}
	if info, err := os.Stat(filepath.Join(dir, "packed-refs")); err == nil {
		return records + info.Size(), nil
	} else if !os.IsNotExist(err) {
		return 0, err
	}
	return records, nil
}
