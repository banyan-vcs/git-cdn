package cache

import (
	"context"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

var refStorageFormats = []string{"reftable", "files"}

// staleRefLocks plants what a Git process killed inside a reference
// transaction leaves in a scope: the stack lock of a reftable repository, or
// the packed-refs and HEAD locks of a files repository plus a lock for each
// named reference it was about to move.
func staleRefLocks(t *testing.T, dir, format string, refs ...string) {
	t.Helper()
	if format == "reftable" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "reftable", "tables.list.lock"), nil, 0600))
		return
	}
	for _, ref := range append([]string{"HEAD", "packed-refs"}, refs...) {
		path := filepath.Join(dir, filepath.FromSlash(ref)+".lock")
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0700))
		require.NoError(t, os.WriteFile(path, nil, 0600))
	}
}

// staleTables plants the tables a killed reftable writer leaves outside the
// table list: the temporary file it was writing and the finished table it
// had renamed into place without publishing the list.
func staleTables(t *testing.T, dir string) {
	t.Helper()
	for _, name := range []string{"0x000000000000-0x000000000000-00000000.temp.aB3dEf", "0x000000000000-0x000000000000-00000000.ref"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, "reftable", name), nil, 0600))
	}
}

// requireListedTablesOnly fails for any file in the reftable directory that
// is neither the table list nor a table it names.
func requireListedTablesOnly(t *testing.T, dir string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "reftable", "tables.list"))
	require.NoError(t, err)
	listed := make(map[string]bool)
	for _, line := range strings.Fields(string(data)) {
		listed[line] = true
	}
	entries, err := os.ReadDir(filepath.Join(dir, "reftable"))
	require.NoError(t, err)
	for _, entry := range entries {
		require.True(t, entry.Name() == "tables.list" || listed[entry.Name()], "%s is not a published table", entry.Name())
	}
}

func requireNoRefLocks(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasSuffix(entry.Name(), ".lock") {
			return fmt.Errorf("%s survived", path)
		}
		return nil
	})
	require.NoError(t, err)
}

func snapshotOf(ref, name string) protocol.Snapshot {
	return protocol.Snapshot{Refs: []protocol.Ref{{Name: "HEAD", OID: ref, Symref: name}, {Name: name, OID: ref}}}
}

// Every reference transaction in a scope runs under the exclusive gate, so
// whatever a killed writer left there belongs to a process that has already
// been reaped. The scope must keep writing references instead of failing
// until the files are removed by hand.
func TestRefWritesDiscardLocksOfAKilledWriter(t *testing.T) {
	for _, format := range refStorageFormats {
		t.Run(format, func(t *testing.T) {
			s, l, roots := populatedLease(t, 32, format)
			defer l.Release()
			latest, older := roots[len(roots)-1], roots[len(roots)-2]

			// Reconciliation: the leftovers sit on the refs the transaction moves.
			staleRefLocks(t, l.Dir, format, "refs/cache/roots/"+latest, "refs/cache/upstream/heads/main")
			if format == "reftable" {
				staleTables(t, l.Dir)
			}
			require.NoError(t, l.SyncRefs(context.Background(), snapshotOf(older, "refs/heads/main")))
			require.Equal(t, "refs/cache/upstream/heads/main", maintenanceGit(t, s.Git, l.Dir, "symbolic-ref", "HEAD"))
			require.Equal(t, older, maintenanceGit(t, s.Git, l.Dir, "rev-parse", "refs/cache/roots/"+older))
			require.Equal(t, latest, maintenanceGit(t, s.Git, l.Dir, "rev-parse", retainedPrefix+latest))
			require.Equal(t, "", maintenanceGit(t, s.Git, l.Dir, "for-each-ref", "--format=%(refname)", "refs/cache/roots/"+latest))
			requireNoRefLocks(t, l.Dir)
			if format == "reftable" {
				requireListedTablesOnly(t, l.Dir)
			}

			// Reconciliation that only moves HEAD meets the lock alone.
			staleRefLocks(t, l.Dir, format)
			require.NoError(t, l.SyncRefs(context.Background(), snapshotOf(older, "refs/heads/develop")))
			require.Equal(t, "refs/cache/upstream/heads/develop", maintenanceGit(t, s.Git, l.Dir, "symbolic-ref", "HEAD"))
			requireNoRefLocks(t, l.Dir)

			// Hydration publishes its roots through the same kind of transaction.
			work := filepath.Join(t.TempDir(), "work")
			maintenanceGit(t, s.Git, "", "init", "-q", "--object-format=sha1", "--initial-branch=main", work)
			require.NoError(t, os.WriteFile(filepath.Join(work, "data"), []byte("later revision\n"), 0600))
			maintenanceGit(t, s.Git, work, "add", "data")
			maintenanceGit(t, s.Git, work, "commit", "-q", "-m", "later revision")
			fresh := maintenanceGit(t, s.Git, work, "rev-parse", "HEAD")
			staleRefLocks(t, l.Dir, format, "refs/cache/roots/"+fresh)
			hit, err := l.Ensure(context.Background(), (&url.URL{Scheme: "file", Path: work}).String(), nil, []string{fresh})
			require.NoError(t, err)
			require.False(t, hit)
			require.Equal(t, fresh, maintenanceGit(t, s.Git, l.Dir, "rev-parse", "refs/cache/roots/"+fresh))
			requireNoRefLocks(t, l.Dir)
		})
	}
}

// A files repository is only asked about the references a transaction
// writes. A lock elsewhere neither fails that transaction nor survives the
// one that does touch its reference.
func TestRefWritesLeaveLocksTheyDoNotNeed(t *testing.T) {
	_, l, roots := populatedLease(t, 32, "files")
	defer l.Release()
	latest := roots[len(roots)-1]
	untouched := filepath.Join(l.Dir, "refs", "cache", "upstream", "heads", "topic.lock")
	require.NoError(t, os.MkdirAll(filepath.Dir(untouched), 0700))
	require.NoError(t, os.WriteFile(untouched, nil, 0600))

	require.NoError(t, l.SyncRefs(context.Background(), snapshotOf(latest, "refs/heads/main")))
	_, err := os.Stat(untouched)
	require.NoError(t, err, "a lock on a reference outside the transaction was removed")

	withTopic := snapshotOf(latest, "refs/heads/main")
	withTopic.Refs = append(withTopic.Refs, protocol.Ref{Name: "refs/heads/topic", OID: latest})
	require.NoError(t, l.SyncRefs(context.Background(), withTopic))
	require.Equal(t, latest, maintenanceGit(t, l.store.Git, l.Dir, "rev-parse", "refs/cache/upstream/heads/topic"))
	requireNoRefLocks(t, l.Dir)
}

// Collecting expired grace pins deletes references too; a lock left by an
// earlier writer must not keep the pins, and the history they protect,
// forever.
func TestGCExpiresGracePinsOverAKilledRefWriter(t *testing.T) {
	for _, format := range refStorageFormats {
		t.Run(format, func(t *testing.T) {
			s, l, roots := populatedLease(t, 32, format)
			defer l.Release()
			now := time.Now()
			oldRoots := roots[:len(roots)-1]
			retired := oldRoots[len(oldRoots)-1]
			require.NoError(t, l.SyncRefs(context.Background(), snapshotOf(roots[len(roots)-1], "refs/heads/main")))
			_, err := l.rewrite(context.Background(), true, now.Add(13*24*time.Hour))
			require.NoError(t, err)

			staleRefLocks(t, l.Dir, format, retainedPrefix+retired)
			if format == "reftable" {
				staleTables(t, l.Dir)
			}
			_, err = l.rewrite(context.Background(), true, now.Add(15*24*time.Hour))
			require.NoError(t, err)
			retained, err := readRetention(l.Dir)
			require.NoError(t, err)
			require.Empty(t, retained)
			require.Equal(t, "", maintenanceGit(t, s.Git, l.Dir, "for-each-ref", "--format=%(refname)", retainedPrefix))
			requireNoRefLocks(t, l.Dir)
			if format == "reftable" {
				requireListedTablesOnly(t, l.Dir)
			}
		})
	}
}
