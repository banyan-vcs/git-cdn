package sshproxy

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// A drained backend refuses new SSH work at the exec gate: the client
// sees the reason on stderr with a failing exit status, the same shape an
// origin error produces, instead of an opaque channel rejection.
func TestDrainedBackendRefusesNewSessions(t *testing.T) {
	f := newSSHFixture(t)
	f.server.backend.SetDraining(true)
	t.Cleanup(func() { f.server.backend.SetDraining(false) })

	address, _, _ := strings.Cut(strings.TrimPrefix(f.base, "ssh://git@"), "/")
	callback, err := knownhosts.New(f.known)
	require.NoError(t, err)
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.Password("fixture-token")},
		HostKeyCallback: callback,
		Timeout:         5 * time.Second,
	})
	require.NoError(t, err)
	defer client.Close()

	session, err := client.NewSession()
	require.NoError(t, err)
	defer session.Close()
	stderr, stdout := &bytes.Buffer{}, &bytes.Buffer{}
	session.Stderr, session.Stdout = stderr, stdout
	err = session.Start("git-upload-pack 'Group/Repo.git'")
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		require.Error(t, err, "a drained node served a session")
		var exit *ssh.ExitError
		require.ErrorAs(t, err, &exit)
		require.EqualValues(t, 1, exit.ExitStatus())
		require.Contains(t, stderr.String(), "node is draining")
	case <-time.After(10 * time.Second):
		t.Fatal("the drained session never finished")
	}
	require.Empty(t, stdout.String(), "a drained session produced output")
}
