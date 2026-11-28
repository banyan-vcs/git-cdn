package cache

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// clearStaleRefLocks discards the reference locks a Git process leaves behind
// when it is killed inside a reference transaction: the stack lock of a
// reftable repository, or the HEAD, packed-refs and per-reference locks of a
// files repository. Git removes them itself on exit, but a reconciliation
// preempted by a queued request, a hydration whose client left or a process
// that was stopped does not get that far, and every later transaction in the
// scope then fails with "cannot lock".
//
// The caller owns the repository: it holds the scope's exclusive gate, the
// store's directory lock while migrating, or a staging copy that only this
// maintenance uses. That is what makes every lock it can find stale — Git
// runs in a scope only under a lease, the processes the previous holder
// started had exited before it released, and the directory lock keeps other
// processes out of the cache.
func clearStaleRefLocks(dir string) error {
	info, err := os.Stat(filepath.Join(dir, "reftable"))
	if err == nil && info.IsDir() {
		return removeIfPresent(filepath.Join(dir, "reftable", "tables.list.lock"))
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	for _, name := range []string{"HEAD.lock", "packed-refs.lock"} {
		if err := removeIfPresent(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return filepath.WalkDir(filepath.Join(dir, "refs"), func(path string, entry fs.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			return os.Remove(path)
		}
		return nil
	})
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
