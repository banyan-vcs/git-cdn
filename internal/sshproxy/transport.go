package sshproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type origin struct {
	settings       config.SSHUpstream
	user, repo     string
	sharedIdentity string
	git            *nativegit.Runner
	maxPack        int64
}
type originSession struct {
	client  *ssh.Client
	session *ssh.Session
	in      io.WriteCloser
	out     io.Reader
	err     io.Reader
	stop    chan struct{}
}

func (o *origin) open(ctx context.Context, password []byte, command gitCommand, version string) (*originSession, error) {
	conn, err := (&net.Dialer{Timeout: 15 * time.Second}).DialContext(ctx, "tcp", o.settings.Address)
	if err != nil {
		return nil, fmt.Errorf("origin SSH connection failed")
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-stop:
		}
	}()
	deadline := time.Now().Add(15 * time.Second)
	conn.SetDeadline(deadline)
	handshakeCtx, handshakeCancel := context.WithDeadline(ctx, deadline)
	callback := func(address string, remote net.Addr, key ssh.PublicKey) error {
		if err := verifyHostKey(handshakeCtx, o.settings, address, remote, key); err != nil {
			return fmt.Errorf("%w: %w", errHostKeyCheck, err)
		}
		return nil
	}
	cc, chans, requests, err := ssh.NewClientConn(conn, o.settings.Address, &ssh.ClientConfig{User: o.user, Auth: []ssh.AuthMethod{ssh.Password(string(password))}, HostKeyCallback: callback, HostKeyAlgorithms: o.trustedAlgorithms(conn.RemoteAddr())})
	handshakeCancel()
	if err != nil {
		close(stop)
		conn.Close()
		if errors.Is(err, errHostKeyCheck) {
			return nil, fmt.Errorf("origin SSH host key check failed")
		}
		return nil, fmt.Errorf("origin SSH password authentication failed")
	}
	conn.SetDeadline(time.Time{})
	client := ssh.NewClient(cc, chans, requests)
	session, err := client.NewSession()
	if err != nil {
		close(stop)
		client.Close()
		return nil, fmt.Errorf("origin SSH session failed")
	}
	if version != "" {
		_ = session.Setenv("GIT_PROTOCOL", version)
	}
	in, err := session.StdinPipe()
	if err != nil {
		close(stop)
		client.Close()
		return nil, err
	}
	out, err := session.StdoutPipe()
	if err != nil {
		close(stop)
		client.Close()
		return nil, err
	}
	stderr, err := session.StderrPipe()
	if err != nil {
		close(stop)
		client.Close()
		return nil, err
	}
	exec := command.name + " '" + o.repo + "'"
	if command.operation != "" {
		exec += " " + command.operation
	}
	if err := session.Start(exec); err != nil {
		close(stop)
		client.Close()
		return nil, fmt.Errorf("origin Git command rejected")
	}
	return &originSession{client: client, session: session, in: in, out: out, err: stderr, stop: stop}, nil
}

// Prefer algorithms already trusted for this destination, as OpenSSH does.
// Otherwise a multi-key server could negotiate a different, unpinned key.
func (o *origin) trustedAlgorithms(remote net.Addr) []string {
	// Algorithm selection must never enroll a key from an unrelated host entry.
	check, err := knownhosts.New(o.settings.KnownHosts)
	if err != nil {
		return nil
	}
	data, err := os.ReadFile(o.settings.KnownHosts)
	if err != nil {
		return nil
	}
	var algorithms []string
	seen := map[string]bool{}
	for len(data) > 0 {
		_, _, key, _, rest, err := ssh.ParseKnownHosts(data)
		if err != nil {
			break
		}
		data = rest
		if check(o.settings.Address, remote, key) != nil {
			continue
		}
		values := []string{key.Type()}
		if key.Type() == ssh.KeyAlgoRSA {
			values = []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
		}
		for _, value := range values {
			if !seen[value] {
				seen[value] = true
				algorithms = append(algorithms, value)
			}
		}
	}
	return algorithms
}

func (s *originSession) close() {
	close(s.stop)
	// The input-copy goroutine owns EOF; a second CloseWrite would race it.
	s.session.Close()
	s.client.Close()
}

func (o *origin) refs(ctx context.Context, password []byte, maxRefs int64) (protocol.Snapshot, error) {
	s, err := o.open(ctx, password, gitCommand{name: "git-upload-pack"}, "version=2")
	if err != nil {
		return protocol.Snapshot{}, err
	}
	defer s.close()
	go io.Copy(io.Discard, s.err)
	advertisement, err := readMessage(s.out, 1<<20)
	if err != nil || !bytes.Contains(advertisement, []byte("version 2\n")) {
		return protocol.Snapshot{}, fmt.Errorf("origin does not support SSH Git v2")
	}
	observed := time.Now()
	if _, err := s.in.Write(protocol.LSRefsRequest()); err != nil {
		return protocol.Snapshot{}, err
	}
	data, err := readMessage(s.out, maxRefs)
	if err != nil {
		return protocol.Snapshot{}, err
	}
	refs, err := protocol.ParseRefs(data)
	refs.ObservedAt = observed
	return refs, err
}

func (o *origin) hydrate(ctx context.Context, password []byte, stage string, roots []string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s, err := o.open(ctx, password, gitCommand{name: "git-upload-pack"}, "version=2")
	if err != nil {
		return err
	}
	defer s.close()
	go io.Copy(io.Discard, s.err)
	advertisement, err := readMessage(s.out, 1<<20)
	if err != nil || !bytes.Contains(advertisement, []byte("version 2\n")) {
		return fmt.Errorf("origin SSH hydration requires Git v2")
	}
	request := append(protocol.Encode("command=fetch\n"), []byte("0001")...)
	request = append(request, protocol.Encode("thin-pack\n")...)
	request = append(request, protocol.Encode("ofs-delta\n")...)
	for _, root := range roots {
		request = append(request, protocol.Encode("want "+root+"\n")...)
	}
	refs, err := o.git.Output(ctx, stage, "", "for-each-ref", "--format=%(objectname)", "refs/cache/roots/")
	if err != nil {
		return err
	}
	count := 0
	for _, oid := range strings.Fields(string(refs)) {
		if count >= 128 {
			break
		}
		kind, err := o.git.Output(ctx, stage, "", "cat-file", "-t", oid)
		if err != nil || string(kind) != "commit\n" {
			continue
		}
		request = append(request, protocol.Encode("have "+oid+"\n")...)
		count++
	}
	request = append(request, protocol.Encode("done\n")...)
	request = append(request, []byte("0000")...)
	if _, err := s.in.Write(request); err != nil {
		return err
	}
	reader, writer := io.Pipe()
	result := make(chan error, 1)
	go func() { err := receivePack(s.out, writer, o.maxPack); writer.CloseWithError(err); result <- err }()
	indexErr := o.git.Run(ctx, stage, reader, io.Discard, nil, "index-pack", "--stdin", "--fix-thin", "--strict")
	reader.Close()
	if indexErr != nil {
		cancel()
	}
	receiveErr := <-result
	if indexErr != nil {
		return indexErr
	}
	if receiveErr != nil {
		return receiveErr
	}
	var updates strings.Builder
	for _, root := range roots {
		fmt.Fprintf(&updates, "update refs/cache/roots/%s %s\n", root, root)
	}
	return o.git.Run(ctx, stage, strings.NewReader(updates.String()), io.Discard, nil, "update-ref", "--stdin")
}

func receivePack(input io.Reader, output io.Writer, max int64) error {
	var written int64
	inPack := false
	for {
		p, err := readPacket(input)
		if err != nil {
			return err
		}
		if p.control == 0 || p.control == 2 {
			break
		}
		if bytes.Equal(p.data, []byte("packfile\n")) {
			inPack = true
			continue
		}
		if !inPack || len(p.data) == 0 {
			continue
		}
		switch p.data[0] {
		case 1:
			written += int64(len(p.data) - 1)
			if written > max {
				return fmt.Errorf("origin pack exceeds cache budget")
			}
			if _, err := output.Write(p.data[1:]); err != nil {
				return err
			}
		case 3:
			return fmt.Errorf("origin reported a Git fetch failure")
		}
	}
	if !inPack || written == 0 {
		return fmt.Errorf("origin did not send a pack")
	}
	return nil
}

// identity returns the cache scope for this repository. A shared identity
// derives from the HTTP upstream in single-origin deployments; an explicit
// cache_identity_url pins the scope manually; otherwise SSH state is keyed
// by address, upstream username and path.
func (o *origin) identity() string {
	if o.sharedIdentity != "" {
		return o.sharedIdentity
	}
	if o.settings.CacheIdentityURL != "" {
		return strings.TrimSuffix(o.settings.CacheIdentityURL, "/") + "/" + o.repo + ":sha1"
	}
	u := url.URL{Scheme: "ssh", Host: o.settings.Address, User: url.User(o.user), Path: "/" + o.repo}
	return u.String() + ":sha1"
}

func makeOrigin(settings config.SSHUpstream, user, repo string, g *nativegit.Runner, maxPack int64) (*origin, error) {
	if settings.User != "" {
		user = settings.User
	}
	if settings.PathPrefix != "" {
		repo = strings.Trim(settings.PathPrefix, "/") + "/" + repo
	}
	if err := config.ValidateRepositoryPath(repo); err != nil {
		return nil, err
	}
	settings.KnownHosts = filepath.Clean(settings.KnownHosts)
	_, err := loadHostKeys(settings.KnownHosts, settings.HostKeyPolicy == config.SSHHostKeyAcceptNew)
	if err != nil {
		return nil, fmt.Errorf("cannot load origin SSH known_hosts")
	}
	return &origin{settings: settings, user: user, repo: repo, git: g, maxPack: maxPack}, nil
}
