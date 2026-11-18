package server

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFullClonePrunesRemovedBranchesTagsAndCacheRoots(t *testing.T) {
	f := newFixture(t)
	old := f.tip
	testGit(t, f.repo, "update-ref", "refs/heads/obsolete", old)
	s, h := newCDN(t, f, nil)
	testGit(t, "", "clone", "--quiet", h.URL+"/gitlab/Group/Repo.git", filepath.Join(t.TempDir(), "first"))
	key := s.store.Key(f.origin.URL + "/root/Group/Repo.git:sha1")
	l, err := s.store.Acquire(context.Background(), key)
	require.NoError(t, err)
	require.Equal(t, old, testGit(t, l.Dir, "rev-parse", "refs/cache/upstream/heads/obsolete"))
	oldTag := testGit(t, l.Dir, "rev-parse", "refs/cache/upstream/tags/v1")
	l.Release()
	testGit(t, f.work, "checkout", "--quiet", "--orphan", "replacement")
	testGit(t, f.work, "rm", "--quiet", "-rf", ".")
	f.commit(t, "replacement")
	testGit(t, f.work, "push", "--quiet", "--force", f.repo, "HEAD:main")
	testGit(t, f.repo, "update-ref", "-d", "refs/heads/obsolete")
	testGit(t, f.repo, "update-ref", "-d", "refs/tags/v1")
	testGit(t, "", "clone", "--quiet", h.URL+"/gitlab/Group/Repo.git", filepath.Join(t.TempDir(), "second"))
	l, err = s.store.Acquire(context.Background(), key)
	require.NoError(t, err)
	defer l.Release()
	require.Equal(t, f.tip, testGit(t, l.Dir, "rev-parse", "refs/cache/upstream/heads/main"))
	require.Equal(t, "refs/cache/upstream/heads/main", testGit(t, l.Dir, "symbolic-ref", "HEAD"))
	for _, ref := range []string{"refs/cache/upstream/heads/obsolete", "refs/cache/upstream/tags/v1", "refs/cache/roots/" + old, "refs/cache/roots/" + oldTag} {
		_, err := s.git.Output(context.Background(), l.Dir, "", "show-ref", "--verify", ref)
		require.Error(t, err, ref)
	}
}

func TestSingleBranchFullCloneDoesNotHydrateUnrequestedRefs(t *testing.T) {
	f := newFixture(t)
	main := f.tip
	testGit(t, f.work, "checkout", "--quiet", "--orphan", "other")
	testGit(t, f.work, "rm", "--quiet", "-rf", ".")
	f.commit(t, "other branch")
	other := f.tip
	testGit(t, f.work, "push", "--quiet", f.repo, "HEAD:refs/heads/other")
	testGit(t, f.work, "checkout", "--quiet", "main")
	f.tip = main
	s, h := newCDN(t, f, nil)
	testGit(t, "", "clone", "--quiet", "--single-branch", "--branch", "main", "--no-tags", h.URL+"/gitlab/Group/Repo.git", filepath.Join(t.TempDir(), "client"))
	l, err := s.store.Acquire(context.Background(), s.store.Key(f.origin.URL+"/root/Group/Repo.git:sha1"))
	require.NoError(t, err)
	defer l.Release()
	require.Equal(t, main, testGit(t, l.Dir, "rev-parse", "refs/cache/upstream/heads/main"))
	_, err = s.git.Output(context.Background(), l.Dir, "", "cat-file", "-e", other)
	require.Error(t, err)
	_, err = s.git.Output(context.Background(), l.Dir, "", "show-ref", "--verify", "refs/cache/upstream/heads/other")
	require.Error(t, err)
}
