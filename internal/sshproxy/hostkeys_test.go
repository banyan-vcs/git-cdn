package sshproxy

import (
	"context"
	"crypto/rand"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestHostKeyEnrollmentPersistenceAndMismatch(t *testing.T) {
	settings := config.SSHUpstream{KnownHosts: filepath.Join(t.TempDir(), "trust", "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
	signer, _ := hostKey(t, t.TempDir(), "key")
	other, _ := hostKey(t, t.TempDir(), "other")
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 2222}
	ctx := context.Background()
	for _, address := range []string{"git.example:22", "git.example:2222", "[::1]:2222"} {
		require.NoError(t, verifyHostKey(ctx, settings, address, remote, signer.PublicKey()))
	}
	data, err := os.ReadFile(settings.KnownHosts)
	require.NoError(t, err)
	require.Len(t, strings.Split(strings.TrimSpace(string(data)), "\n"), 3)
	for _, address := range []string{"git.example:22", "git.example:2222", "[::1]:2222"} {
		require.NoError(t, verifyHostKey(ctx, settings, address, remote, signer.PublicKey()))
		require.Error(t, verifyHostKey(ctx, settings, address, remote, other.PublicKey()))
	}
	after, err := os.ReadFile(settings.KnownHosts)
	require.NoError(t, err)
	require.Equal(t, data, after, "known keys were duplicated or replaced")
	info, err := os.Stat(settings.KnownHosts)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	settings.HostKeyPolicy = config.SSHHostKeyStrict
	require.NoError(t, verifyHostKey(ctx, settings, "git.example:22", remote, signer.PublicKey()))
	require.Error(t, verifyHostKey(ctx, settings, "new.example:22", remote, signer.PublicKey()))
}

func TestHostKeyEnrollmentPreservesExistingTrust(t *testing.T) {
	key, _ := hostKey(t, t.TempDir(), "key")
	other, _ := hostKey(t, t.TempDir(), "other")
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	for _, test := range []struct {
		name, contents string
		key            ssh.PublicKey
		accept         bool
	}{
		{"missing strict trust", "", key.PublicKey(), false},
		{"malformed trust", "invalid known_hosts entry", key.PublicKey(), false},
		{"revoked key", "@revoked " + knownhosts.Line([]string{"other.example"}, key.PublicKey()), key.PublicKey(), false},
		{"hashed mismatch", knownhosts.Line([]string{knownhosts.HashHostname("git.example")}, key.PublicKey()), other.PublicKey(), false},
		{"preserve comments and missing newline", "# verified entry\n" + knownhosts.Line([]string{"other.example"}, other.PublicKey()), key.PublicKey(), true},
	} {
		t.Run(test.name, func(t *testing.T) {
			settings := config.SSHUpstream{KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
			require.NoError(t, os.WriteFile(settings.KnownHosts, []byte(test.contents), 0644))
			if test.name == "missing strict trust" {
				settings.HostKeyPolicy = config.SSHHostKeyStrict
			}
			err := verifyHostKey(context.Background(), settings, "git.example:22", remote, test.key)
			data, readErr := os.ReadFile(settings.KnownHosts)
			require.NoError(t, readErr)
			if test.accept {
				require.NoError(t, err)
				require.True(t, strings.HasPrefix(string(data), test.contents+"\n"))
				info, err := os.Stat(settings.KnownHosts)
				require.NoError(t, err)
				require.Equal(t, os.FileMode(0644), info.Mode().Perm())
			} else {
				require.Error(t, err)
				require.Equal(t, test.contents, string(data))
			}
		})
	}
}

func TestConcurrentHostKeyEnrollment(t *testing.T) {
	settings := config.SSHUpstream{KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
	signer, _ := hostKey(t, t.TempDir(), "key")
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	// The enrollments take the lock in turn and each one syncs a file and its
	// directory; on a loaded host that alone can take seconds. This test is
	// about the outcome, not the speed.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var workers sync.WaitGroup
	results := make(chan error, 24)
	for i := range 24 {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			results <- verifyHostKey(ctx, settings, fmt.Sprintf("host-%d.example:22", i%12), remote, signer.PublicKey())
		}(i)
	}
	workers.Wait()
	close(results)
	for err := range results {
		require.NoError(t, err)
	}
	data, err := os.ReadFile(settings.KnownHosts)
	require.NoError(t, err)
	require.Len(t, strings.Split(strings.TrimSpace(string(data)), "\n"), 12)
	settings.HostKeyPolicy = config.SSHHostKeyStrict
	for i := range 12 {
		require.NoError(t, verifyHostKey(ctx, settings, fmt.Sprintf("host-%d.example:22", i), remote, signer.PublicKey()))
	}
}

func TestHostKeyEnrollmentLockDeadline(t *testing.T) {
	settings := config.SSHUpstream{KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
	lock, err := lockHostKeys(context.Background(), settings.KnownHosts+".lock")
	require.NoError(t, err)
	defer lock.Close()
	signer, _ := hostKey(t, t.TempDir(), "key")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err = verifyHostKey(ctx, settings, "git.example:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, signer.PublicKey())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	_, err = os.Stat(settings.KnownHosts)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestConcurrentConflictingHostKeysOnlyPinOneKey(t *testing.T) {
	settings := config.SSHUpstream{KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
	first, _ := hostKey(t, t.TempDir(), "first")
	second, _ := hostKey(t, t.TempDir(), "second")
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, key := range []ssh.PublicKey{first.PublicKey(), second.PublicKey()} {
		go func(key ssh.PublicKey) {
			<-start
			results <- verifyHostKey(ctx, settings, "git.example:22", remote, key)
		}(key)
	}
	close(start)
	one, two := <-results, <-results
	require.NotEqual(t, one == nil, two == nil, "conflicting first-use keys must not both be accepted")
	data, err := os.ReadFile(settings.KnownHosts)
	require.NoError(t, err)
	require.Len(t, strings.Split(strings.TrimSpace(string(data)), "\n"), 1)
}

func TestAlgorithmSelectionDoesNotEnrollUnrelatedKey(t *testing.T) {
	settings := config.SSHUpstream{Address: "git.example:22", KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
	signer, _ := hostKey(t, t.TempDir(), "key")
	data := []byte(knownhosts.Line([]string{"other.example"}, signer.PublicKey()) + "\n")
	require.NoError(t, os.WriteFile(settings.KnownHosts, data, 0600))
	o, err := makeOrigin(settings, "git", "repo.git", nil, 0, nil)
	require.NoError(t, err)
	require.Empty(t, o.trustedAlgorithms(&net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}))
	after, err := os.ReadFile(settings.KnownHosts)
	require.NoError(t, err)
	require.Equal(t, data, after)
}

func TestHostKeyEnrollmentDoesNotTrustUnknownCertificate(t *testing.T) {
	settings := config.SSHUpstream{KnownHosts: filepath.Join(t.TempDir(), "known_hosts"), HostKeyPolicy: config.SSHHostKeyAcceptNew}
	signer, _ := hostKey(t, t.TempDir(), "host")
	ca, _ := hostKey(t, t.TempDir(), "ca")
	certificate := &ssh.Certificate{Key: signer.PublicKey(), CertType: ssh.HostCert, ValidPrincipals: []string{"git.example"}, ValidBefore: ssh.CertTimeInfinity}
	require.NoError(t, certificate.SignCert(rand.Reader, ca))
	err := verifyHostKey(context.Background(), settings, "git.example:22", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}, certificate)
	require.Error(t, err)
	_, err = os.Stat(settings.KnownHosts)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestHostKeyEnrollmentDoesNotReplaceSpecialFiles(t *testing.T) {
	signer, _ := hostKey(t, t.TempDir(), "key")
	settings := config.SSHUpstream{KnownHosts: os.DevNull, HostKeyPolicy: config.SSHHostKeyAcceptNew}
	remote := &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 22}
	require.Error(t, verifyHostKey(context.Background(), settings, "git.example:22", remote, signer.PublicKey()))
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	require.NoError(t, os.WriteFile(target, nil, 0600))
	settings.KnownHosts = filepath.Join(dir, "known_hosts")
	require.NoError(t, os.Symlink(target, settings.KnownHosts))
	require.Error(t, verifyHostKey(context.Background(), settings, "git.example:22", remote, signer.PublicKey()))
	link, err := os.Readlink(settings.KnownHosts)
	require.NoError(t, err)
	require.Equal(t, target, link)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Empty(t, data)
}
