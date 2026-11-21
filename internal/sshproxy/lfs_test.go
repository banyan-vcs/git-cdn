package sshproxy

import (
	"bytes"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func lfsClient(t *testing.T, f *sshFixture, token string) *ssh.Client {
	t.Helper()
	address, _, _ := strings.Cut(strings.TrimPrefix(f.base, "ssh://git@"), "/")
	check, err := knownhosts.New(f.known)
	require.NoError(t, err)
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.Password(token)}, HostKeyCallback: check})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client
}

func TestLFSCommandValidation(t *testing.T) {
	for _, name := range []string{"git-lfs-authenticate", "git-lfs-transfer"} {
		for _, operation := range []string{"download", "upload"} {
			for _, repo := range []string{"Group/Repo", "'Group/Repo.git'", "'/Group/Repo'", `"Group/Repo.git"`} {
				command, err := parseCommand(name + " " + repo + " " + operation)
				require.NoError(t, err)
				require.Equal(t, gitCommand{name: name, repo: "Group/Repo.git", operation: operation}, command)
			}
		}
		for _, args := range []string{"Group/Repo", "Group/Repo delete", "Group/Repo download extra", "Group/Repo download;id", "'../Repo' download", "'Repo;id' upload", "'Group/Repo'\ndownload", "'Group/Repo' ''"} {
			_, err := parseCommand(name + " " + args)
			require.Error(t, err, name+" "+args)
		}
	}
	command, err := parseCommand("git-lfs-authenticate 'Group/Repo' 'download'")
	require.NoError(t, err)
	require.Equal(t, "download", command.operation)
	command, err = parseCommand(`git-lfs-transfer "Group/Repo" "upload"`)
	require.NoError(t, err)
	require.Equal(t, "upload", command.operation)
}

func TestLFSAuthenticatePreservesOriginResponseAndRevocation(t *testing.T) {
	response := []byte("{\"href\":\"https://lfs.example.com/Group/Repo.git/info/lfs\",\"header\":{\"Authorization\":\"Bearer opaque-lfs-token\"},\"expires_in\":300,\"future\":{\"signed\":\"a%2Bb%2Fc\"}}\n")
	var calls atomic.Int64
	f := newSSHFixtureWithLFS(t, func(ch ssh.Channel, command gitCommand) uint32 {
		if command.name != "git-lfs-authenticate" || command.repo != "Group/Repo.git" {
			return 1
		}
		calls.Add(1)
		ch.Write(response)
		return 0
	})
	client := lfsClient(t, f, "fixture-token")
	for _, operation := range []string{"download", "upload"} {
		session, err := client.NewSession()
		require.NoError(t, err)
		output, err := session.Output("git-lfs-authenticate '/Group/Repo' " + operation)
		session.Close()
		require.NoError(t, err)
		require.Equal(t, response, output)
	}
	f.denied.Store(true)
	session, err := client.NewSession()
	require.NoError(t, err)
	output, err := session.Output("git-lfs-authenticate 'Group/Repo.git' download")
	session.Close()
	require.Error(t, err)
	require.Empty(t, output)
	require.EqualValues(t, 2, calls.Load())
	local, forwarded, hydration := f.server.Stats()
	require.Zero(t, local)
	require.Zero(t, hydration)
	require.EqualValues(t, 2, forwarded)
}

func TestLFSTransferStreamsBinaryEOFStderrAndExitStatus(t *testing.T) {
	data := bytes.Repeat([]byte{0, 255, 1, '\n', 127}, 256<<10)
	greeting := []byte("000eversion 1\n0000")
	received := make(chan []byte, 2)
	f := newSSHFixtureWithLFS(t, func(ch ssh.Channel, command gitCommand) uint32 {
		if command.name != "git-lfs-transfer" || command.repo != "Group/Repo.git" {
			return 1
		}
		ch.Write(greeting)
		body, err := io.ReadAll(ch)
		if err != nil {
			return 1
		}
		received <- body
		ch.Write(body)
		io.WriteString(ch.Stderr(), "origin LFS transfer status\n")
		return 37
	}, func(c *config.Config) { c.MaxRequestBytes = 1024 })
	client := lfsClient(t, f, "fixture-token")
	for _, operation := range []string{"download", "upload"} {
		session, err := client.NewSession()
		require.NoError(t, err)
		var output, stderr bytes.Buffer
		session.Stdin, session.Stdout, session.Stderr = bytes.NewReader(data), &output, &stderr
		err = session.Run("git-lfs-transfer 'Group/Repo.git' " + operation)
		session.Close()
		var exit *ssh.ExitError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, 37, exit.ExitStatus())
		require.Equal(t, append(append([]byte{}, greeting...), data...), output.Bytes())
		require.Equal(t, data, <-received)
		require.Equal(t, "origin LFS transfer status\n", stderr.String())
	}
	local, forwarded, hydration := f.server.Stats()
	require.Zero(t, local)
	require.Zero(t, hydration)
	require.EqualValues(t, 2, forwarded)
}

func TestLFSRoutingAndInvalidCommandsDoNotReachOrigin(t *testing.T) {
	f := newSSHFixtureWithLFS(t, func(ch ssh.Channel, command gitCommand) uint32 {
		if command.repo != "mirror/Group/Repo.git" || command.operation != "download" {
			return 1
		}
		io.WriteString(ch, "routed\n")
		return 0
	}, func(c *config.Config) {
		upstream := *c.SSH.Upstream
		upstream.PathPrefix = "mirror"
		c.SSH.Mode, c.SSH.Upstream = config.ModeMulti, nil
		c.SSH.Upstreams = map[string]config.SSHUpstream{"site": upstream}
	})
	client := lfsClient(t, f, "fixture-token")
	session, err := client.NewSession()
	require.NoError(t, err)
	output, err := session.Output("git-lfs-authenticate 'site/Group/Repo' download")
	session.Close()
	require.NoError(t, err)
	require.Equal(t, "routed\n", string(output))
	before := f.passwordAttempts.Load()
	for _, command := range []string{"git-lfs-authenticate 'unknown/Group/Repo' download", "git-lfs-authenticate 'site/Group/Repo' download;id", "git-lfs-transfer 'site/../Repo' upload", "git-lfs-authenticate 'site/-repo' download extra"} {
		session, err := client.NewSession()
		require.NoError(t, err)
		_, err = session.Output(command)
		session.Close()
		require.Error(t, err)
	}
	require.Equal(t, before, f.passwordAttempts.Load())
	upstream := f.server.cfg.SSH.Upstreams["site"]
	upstream.PathPrefix = ""
	_, err = makeOrigin(upstream, "git", "-repo.git", f.server.git, 0)
	require.Error(t, err, "a site prefix must not hide a leading Git option")
}
