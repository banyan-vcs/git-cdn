package sshproxy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/chiyutianyi/git-cdn/internal/config"
	"github.com/chiyutianyi/git-cdn/internal/nativegit"
	"github.com/chiyutianyi/git-cdn/internal/server"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func hostKey(t *testing.T, dir, name string) (ssh.Signer, string) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	signer, err := ssh.NewSignerFromKey(key)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(key, "test")
	require.NoError(t, err)
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(block), 0600))
	return signer, path
}

func gitFixture(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = nativegit.Environment(map[string]string{"GIT_AUTHOR_NAME": "SSH Test", "GIT_AUTHOR_EMAIL": "test@example.invalid", "GIT_COMMITTER_NAME": "SSH Test", "GIT_COMMITTER_EMAIL": "test@example.invalid"})
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

type sshFixture struct {
	work, repo, base, known, askpass string
	server                           *Server
	denied                           atomic.Bool
	connections                      atomic.Int64
}

func newSSHFixture(t *testing.T) *sshFixture {
	t.Helper()
	root := t.TempDir()
	f := &sshFixture{work: filepath.Join(root, "work"), repo: filepath.Join(root, "origin.git")}
	gitFixture(t, "", "init", "-q", "--initial-branch=main", f.work)
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "file"), []byte("initial\n"), 0600))
	gitFixture(t, f.work, "add", "file")
	gitFixture(t, f.work, "commit", "-q", "-m", "initial")
	gitFixture(t, "", "clone", "-q", "--bare", f.work, f.repo)
	gitFixture(t, f.repo, "config", "uploadpack.allowFilter", "true")
	gitFixture(t, f.repo, "config", "uploadpack.allowAnySHA1InWant", "true")
	signer, _ := hostKey(t, root, "origin-key")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	var mu sync.Mutex
	connections := map[net.Conn]bool{}
	g, err := nativegit.New("git", 4)
	require.NoError(t, err)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			connections[conn] = true
			workers.Add(1)
			mu.Unlock()
			go func() {
				defer workers.Done()
				defer conn.Close()
				defer func() { mu.Lock(); delete(connections, conn); mu.Unlock() }()
				cfg := &ssh.ServerConfig{PasswordCallback: func(meta ssh.ConnMetadata, password []byte) (*ssh.Permissions, error) {
					if f.denied.Load() || string(password) != "fixture-token" {
						return nil, fmt.Errorf("denied")
					}
					return &ssh.Permissions{}, nil
				}}
				cfg.AddHostKey(signer)
				client, channels, requests, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer client.Close()
				go ssh.DiscardRequests(requests)
				f.connections.Add(1)
				for newChannel := range channels {
					ch, requests, err := newChannel.Accept()
					if err != nil {
						continue
					}
					version := ""
					for request := range requests {
						if request.Type == "env" {
							var env struct{ Name, Value string }
							ssh.Unmarshal(request.Payload, &env)
							if env.Name == "GIT_PROTOCOL" {
								version = env.Value
							}
							request.Reply(true, nil)
							continue
						}
						if request.Type != "exec" {
							request.Reply(false, nil)
							continue
						}
						var message struct{ Command string }
						ssh.Unmarshal(request.Payload, &message)
						name, path, err := parseCommand(message.Command)
						if err != nil || path != "Group/Repo.git" {
							request.Reply(false, nil)
							ch.Close()
							break
						}
						request.Reply(true, nil)
						go ssh.DiscardRequests(requests)
						err = g.Run(ctx, "", ch, ch, map[string]string{"GIT_PROTOCOL": version}, strings.TrimPrefix(name, "git-"), f.repo)
						status := uint32(0)
						if err != nil {
							status = 1
						}
						ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
						ch.Close()
						break
					}
				}
			}()
		}
	}()
	originKnown := filepath.Join(root, "origin-known")
	require.NoError(t, os.WriteFile(originKnown, []byte(knownhosts.Line([]string{knownhosts.Normalize(listener.Addr().String())}, signer.PublicKey())+"\n"), 0600))
	cdnSigner, key := hostKey(t, root, "cdn-key")
	c := config.Config{CacheDir: filepath.Join(root, "cache"), Upstream: &config.Upstream{URL: "https://git.example.com"}, SSH: &config.SSH{HostKey: key, Upstream: &config.SSHUpstream{Address: listener.Addr().String(), KnownHosts: originKnown}}}
	require.NoError(t, c.Validate())
	backend, err := server.New(c, slog.New(slog.NewTextHandler(io.Discard, nil)))
	require.NoError(t, err)
	f.server, err = New(c, backend)
	require.NoError(t, err)
	cdn, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go f.server.Serve(cdn)
	_, port, _ := net.SplitHostPort(cdn.Addr().String())
	f.base = "ssh://git@127.0.0.1:" + port + "/Group/Repo"
	f.known = filepath.Join(root, "client-known")
	require.NoError(t, os.WriteFile(f.known, []byte(knownhosts.Line([]string{knownhosts.Normalize(cdn.Addr().String())}, cdnSigner.PublicKey())+"\n"), 0600))
	f.askpass = filepath.Join(root, "askpass")
	require.NoError(t, os.WriteFile(f.askpass, []byte("#!/bin/sh\nprintf '%s' fixture-token\n"), 0700))
	t.Cleanup(func() {
		cdn.Close()
		f.server.Close()
		backend.Close()
		listener.Close()
		cancel()
		mu.Lock()
		for conn := range connections {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
	})
	return f
}

func (f *sshFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "protocol.version=2"}, args...)...)
	cmd.WaitDelay = 2 * time.Second
	cmd.Dir = dir
	cmd.Env = nativegit.Environment(map[string]string{"GIT_SSH_COMMAND": "ssh -F /dev/null -o StrictHostKeyChecking=yes -o UserKnownHostsFile=" + f.known + " -o PubkeyAuthentication=no -o PreferredAuthentications=password", "SSH_ASKPASS": f.askpass, "SSH_ASKPASS_REQUIRE": "force", "DISPLAY": "git-cdn-test"})
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func TestSSHCloneWarmReuseAndLazyUpdate(t *testing.T) {
	f := newSSHFixture(t)
	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	f.git(t, "", "clone", "--quiet", f.base, first)
	local, _, hydrations := f.server.Stats()
	require.EqualValues(t, 1, local)
	require.EqualValues(t, 1, hydrations)
	f.git(t, "", "clone", "--quiet", f.base+".git", second)
	local, _, hydrations = f.server.Stats()
	require.EqualValues(t, 2, local)
	require.EqualValues(t, 1, hydrations)
	require.NoError(t, os.WriteFile(filepath.Join(f.work, "file"), []byte("updated\n"), 0600))
	gitFixture(t, f.work, "add", "file")
	gitFixture(t, f.work, "commit", "-q", "-m", "update")
	gitFixture(t, f.work, "push", "-q", f.repo, "main")
	f.git(t, first, "fetch", "--quiet", "origin")
	require.Equal(t, gitFixture(t, f.work, "rev-parse", "HEAD"), gitFixture(t, first, "rev-parse", "origin/main"))
	_, _, hydrations = f.server.Stats()
	require.EqualValues(t, 1, hydrations) // multi-round negotiation is transparently forwarded
	third := filepath.Join(t.TempDir(), "third")
	f.git(t, "", "clone", "--quiet", f.base, third)
	_, _, hydrations = f.server.Stats()
	require.EqualValues(t, 2, hydrations)
}

func TestSSHShallowPassthroughAndRevocation(t *testing.T) {
	f := newSSHFixture(t)
	shallow := filepath.Join(t.TempDir(), "shallow")
	f.git(t, "", "clone", "--quiet", "--depth", "1", f.base, shallow)
	require.Equal(t, "true", gitFixture(t, shallow, "rev-parse", "--is-shallow-repository"))
	_, forwarded, _ := f.server.Stats()
	require.Greater(t, forwarded, uint64(0))
	f.denied.Store(true)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	callback, err := knownhosts.New(f.known)
	require.NoError(t, err)
	u := strings.TrimPrefix(f.base, "ssh://git@")
	address, _, _ := strings.Cut(u, "/")
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	require.NoError(t, err)
	defer conn.Close()
	cc, chans, requests, err := ssh.NewClientConn(conn, address, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.Password("fixture-token")}, HostKeyCallback: callback})
	require.NoError(t, err)
	client := ssh.NewClient(cc, chans, requests)
	defer client.Close()
	session, err := client.NewSession()
	require.NoError(t, err)
	defer session.Close()
	_, err = session.CombinedOutput("git-upload-pack 'Group/Repo.git'")
	require.Error(t, err)
}

func TestSSHCommandValidation(t *testing.T) {
	for _, command := range []string{"sh", "git-upload-pack '../repo'", "git-upload-pack 'repo; echo unsafe'", "git-upload-pack '/../../absolute'", "git-upload-pack 'Group/Repo' extra"} {
		_, _, err := parseCommand(command)
		require.Error(t, err, command)
	}
	_, repo, err := parseCommand("git-upload-pack 'Group/Repo'")
	require.NoError(t, err)
	require.Equal(t, "Group/Repo.git", repo)
}

func TestSSHRejectsPublicKeyAuthenticationWithExplanation(t *testing.T) {
	f := newSSHFixture(t)
	signer, key := hostKey(t, t.TempDir(), "client-key")
	address, _, _ := strings.Cut(strings.TrimPrefix(f.base, "ssh://git@"), "/")
	callback, err := knownhosts.New(f.known)
	require.NoError(t, err)

	t.Run("Go client", func(t *testing.T) {
		var banners []string
		client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
			User:            "git",
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: callback,
			Timeout:         5 * time.Second,
			BannerCallback: func(message string) error {
				banners = append(banners, message)
				return nil
			},
		})
		if client != nil {
			client.Close()
		}
		require.Error(t, err)
		require.Equal(t, []string{passwordOnlyBanner}, banners)
	})

	t.Run("OpenSSH client", func(t *testing.T) {
		host, port, err := net.SplitHostPort(address)
		require.NoError(t, err)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "ssh", "-F", os.DevNull,
			"-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+f.known,
			"-o", "BatchMode=yes", "-o", "IdentitiesOnly=yes",
			"-o", "PreferredAuthentications=publickey", "-o", "PasswordAuthentication=no",
			"-o", "KbdInteractiveAuthentication=no", "-o", "ConnectTimeout=5",
			"-i", key, "-p", port, "git@"+host, "git-upload-pack 'Group/Repo.git'")
		cmd.Env = nativegit.Environment(nil)
		output, err := cmd.CombinedOutput()
		require.NoError(t, ctx.Err())
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, 255, exit.ExitCode())
		require.Contains(t, string(output), strings.TrimSpace(passwordOnlyBanner))
		require.Contains(t, string(output), "Permission denied (password)")
	})

	require.Zero(t, f.connections.Load(), "public-key-only clients reached the origin")
	local, forwarded, hydrations := f.server.Stats()
	require.Zero(t, local)
	require.Zero(t, forwarded)
	require.Zero(t, hydrations)
}

func TestSSHPasswordFallbackWithPublicKeyConfigured(t *testing.T) {
	f := newSSHFixture(t)
	signer, _ := hostKey(t, t.TempDir(), "client-key")
	address, _, _ := strings.Cut(strings.TrimPrefix(f.base, "ssh://git@"), "/")
	callback, err := knownhosts.New(f.known)
	require.NoError(t, err)
	var banners []string
	client, err := ssh.Dial("tcp", address, &ssh.ClientConfig{
		User:            "git",
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer), ssh.Password("fixture-token")},
		HostKeyCallback: callback,
		Timeout:         5 * time.Second,
		BannerCallback: func(message string) error {
			banners = append(banners, message)
			return nil
		},
	})
	require.NoError(t, err)
	client.Close()
	require.Equal(t, []string{passwordOnlyBanner}, banners)
	f.git(t, "", "clone", "--quiet", f.base, filepath.Join(t.TempDir(), "clone"))
	local, _, hydrations := f.server.Stats()
	require.EqualValues(t, 1, local)
	require.EqualValues(t, 1, hydrations)
}
