package sshproxy

import (
	"crypto/sha256"
	"encoding/hex"
	"path/filepath"
	"testing"

	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/stretchr/testify/require"
)

func TestSSHSingleModeSharesHTTPScope(t *testing.T) {
	f := newSSHFixture(t)
	o, err := f.server.route("git", "Group/Repo.git")
	require.NoError(t, err)
	require.Equal(t, "https://git.example.com/Group/Repo.git:sha1", o.identity())
	f.git(t, "", "clone", "--quiet", f.base, filepath.Join(t.TempDir(), "clone"))
	// The scope directory is exactly the one the HTTP frontend derives for
	// the same client-visible repository path.
	sum := sha256.Sum256([]byte("https://git.example.com/Group/Repo.git:sha1"))
	scope := filepath.Join(f.server.cfg.CacheDir, "repos", hex.EncodeToString(sum[:]))
	require.DirExists(t, filepath.Join(scope, "repo.git"))
}

func TestSSHMultiModeKeepsSSHIdentity(t *testing.T) {
	f := newSSHFixture(t, func(c *config.Config) {
		c.SSH.Mode = config.ModeMulti
		c.SSH.Upstreams = map[string]config.SSHUpstream{"sshlab": *c.SSH.Upstream}
		c.SSH.Upstream = nil
	})
	o, err := f.server.route("git", "sshlab/Group/Repo.git")
	require.NoError(t, err)
	address := f.server.cfg.SSH.Upstreams["sshlab"].Address
	require.Equal(t, "ssh://git@"+address+"/Group/Repo.git:sha1", o.identity())
}

func TestSSHExplicitIdentityWinsOverSharedScope(t *testing.T) {
	f := newSSHFixture(t, func(c *config.Config) {
		c.SSH.Upstream.CacheIdentityURL = "https://mirror.example.com/gitlab"
	})
	o, err := f.server.route("git", "Group/Repo.git")
	require.NoError(t, err)
	require.Equal(t, "https://mirror.example.com/gitlab/Group/Repo.git:sha1", o.identity())
}
