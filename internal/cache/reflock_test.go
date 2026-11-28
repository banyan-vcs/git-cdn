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

// Every reference transaction in a scope runs under the exclusive gate, so a
// lock found there belongs to a writer that has already been reaped. The
// scope must keep writing references instead of failing until the file is
// removed by hand.
func TestRefWritesDiscardLocksOfAKilledWriter(t *testing.T) {
	for _, format := range refStorageFormats {
		t.Run(format, func(t *testing.T) {
			s, l, roots := populatedLease(t, 32, format)
			defer l.Release()
			latest, older := roots[len(roots)-1], roots[len(roots)-2]

			// Reconciliation: the lock sits on the refs the transaction moves.
			staleRefLocks(t, l.Dir, format, "refs/cache/roots/"+latest, "refs/cache/upstream/heads/main")
			require.NoError(t, l.SyncRefs(context.Background(), snapshotOf(older, "refs/heads/main")))
			require.Equal(t, "refs/cache/upstream/heads/main", maintenanceGit(t, s.Git, l.Dir, "symbolic-ref", "HEAD"))
			require.Equal(t, older, maintenanceGit(t, s.Git, l.Dir, "rev-parse", "refs/cache/roots/"+older))
			require.Equal(t, latest, maintenanceGit(t, s.Git, l.Dir, "rev-parse", retainedPrefix+latest))
			require.Equal(t, "", maintenanceGit(t, s.Git, l.Dir, "for-each-ref", "--format=%(refname)", "refs/cache/roots/"+latest))

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
			_, err = l.rewrite(context.Background(), true, now.Add(15*24*time.Hour))
			require.NoError(t, err)
			retained, err := readRetention(l.Dir)
			require.NoError(t, err)
			require.Empty(t, retained)
			require.Equal(t, "", maintenanceGit(t, s.Git, l.Dir, "for-each-ref", "--format=%(refname)", retainedPrefix))
			requireNoRefLocks(t, l.Dir)
		})
	}
}
