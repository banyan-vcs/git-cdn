package cache

import (
	"os"
	"path/filepath"
	"strings"
)

// clearStaleRefWrites discards what a Git process killed inside a reference
// transaction leaves in a repository. Git deletes its lock and temporary
// files when a transaction ends and, through its signal handlers, when it is
// terminated; a process killed outright does not get that far, and the lock
// it held makes every later transaction in the repository fail with "cannot
// lock". In a reftable repository these are the stack lock and the tables
// the table list does not name, in a files repository the HEAD, packed-refs
// and per-reference locks.
//
// The caller owns the repository: it holds the scope's exclusive gate, the
// store's directory lock while migrating, or a staging copy that only this
// maintenance uses. That is what makes everything it finds stale — Git runs
// in a scope only under a lease, the processes the previous holder started
// had exited before it released, and the directory lock keeps other
// processes out of the cache.
//
// refs names the references the caller is about to write. A files repository
// keeps one lock per reference, so those are visited instead of every
// directory under refs/; a lock on a reference nobody writes blocks nothing
// and goes with the next transaction that touches it.
func clearStaleRefWrites(dir string, refs []string) error {
	tables := filepath.Join(dir, "reftable")
	if listed, err := os.ReadDir(tables); err == nil {
		return clearStaleTables(tables, listed)
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, ref := range refs {
		if err := removeIfPresent(filepath.Join(dir, filepath.FromSlash(ref)+".lock")); err != nil {
			return err
		}
	}
	// A reference update rewrites packed-refs under its own lock, and a
	// transaction that moves HEAD takes the lock on HEAD.
	for _, name := range []string{"HEAD.lock", "packed-refs.lock"} {
		if err := removeIfPresent(filepath.Join(dir, name)); err != nil {
			return err
		}
	}
	return nil
}

// clearStaleTables removes the stack lock and every table the table list
// does not name: the temporary file a killed writer was filling in, and the
// finished table of a writer killed between renaming it into place and
// publishing the list. Both stay in the scope's byte count otherwise. A
// table the list names is never touched, and without a list nothing but the
// lock is.
func clearStaleTables(tables string, listed []os.DirEntry) error {
	if err := removeIfPresent(filepath.Join(tables, "tables.list.lock")); err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(tables, "tables.list"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	names := make(map[string]bool)
	for _, line := range strings.Split(string(data), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			names[line] = true
		}
	}
	for _, entry := range listed {
		name := entry.Name()
		if name == "tables.list" || names[name] || !entry.Type().IsRegular() {
			continue
		}
		// Bytes that cannot be reclaimed are still counted, nothing more.
		_ = os.Remove(filepath.Join(tables, name))
	}
	return nil
}

func removeIfPresent(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
