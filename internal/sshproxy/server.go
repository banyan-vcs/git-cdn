package sshproxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
	"gitlab.com/banyan-vcs/git-cdn/internal/server"
	"golang.org/x/crypto/ssh"
)

const passwordOnlyBanner = "Git CDN supports SSH password/token authentication only; public-key authentication is not supported.\n" +
	"Use a password/token accepted by the origin, or connect directly to the origin to use SSH keys.\n"

type Server struct {
	cfg                          config.Config
	backend                      *server.Server
	git                          *nativegit.Runner
	signer                       ssh.Signer
	slots                        chan struct{}
	mu                           sync.Mutex
	connections                  map[net.Conn]bool
	closed                       bool
	workers                      sync.WaitGroup
	local, forwarded, hydrations atomic.Uint64
}

func New(c config.Config, backend *server.Server) (*Server, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if c.SSH == nil {
		return nil, fmt.Errorf("ssh configuration required")
	}
	if err := c.SSH.Validate(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(c.SSH.HostKey)
	if err != nil {
		return nil, fmt.Errorf("cannot read SSH host key")
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("invalid SSH host key")
	}
	g := backend.NativeGit()
	return &Server{cfg: c, backend: backend, git: g, signer: signer, slots: make(chan struct{}, c.SSH.MaxConnections), connections: map[net.Conn]bool{}}, nil
}

func (s *Server) Stats() (uint64, uint64, uint64) {
	return s.local.Load(), s.forwarded.Load(), s.hydrations.Load()
}

func (s *Server) Serve(listener net.Listener) error {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			conn.Close()
			return net.ErrClosed
		}
		select {
		case s.slots <- struct{}{}:
			s.connections[conn] = true
			s.workers.Add(1)
			s.mu.Unlock()
			go s.handle(conn)
		default:
			s.mu.Unlock()
			conn.Close()
		}
	}
}

func (s *Server) Close() error {
	s.mu.Lock()
	s.closed = true
	for conn := range s.connections {
		conn.Close()
	}
	s.mu.Unlock()
	s.workers.Wait()
	return nil
}

func (s *Server) handle(conn net.Conn) {
	defer func() {
		conn.Close()
		s.mu.Lock()
		delete(s.connections, conn)
		s.mu.Unlock()
		<-s.slots
		s.workers.Done()
	}()
	idleTimeout, _ := time.ParseDuration(s.cfg.WriteIdleTimeout)
	transport := &idleConn{Conn: conn, timeout: idleTimeout}
	var password []byte
	// Leave PublicKeyCallback unset so only password authentication is offered.
	sshConfig := &ssh.ServerConfig{
		MaxAuthTries:   3,
		BannerCallback: func(ssh.ConnMetadata) string { return passwordOnlyBanner },
		PasswordCallback: func(meta ssh.ConnMetadata, value []byte) (*ssh.Permissions, error) {
			allowed := false
			for _, name := range s.cfg.SSH.AllowedUsers {
				if meta.User() == name {
					allowed = true
				}
			}
			if !allowed || len(value) == 0 || len(value) > 8192 {
				return nil, fmt.Errorf("unsupported SSH credentials")
			}
			password = append(password[:0], value...)
			return &ssh.Permissions{}, nil
		},
	}
	sshConfig.AddHostKey(s.signer)
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	client, channels, requests, err := ssh.NewServerConn(transport, sshConfig)
	if err != nil {
		return
	}
	defer client.Close()
	defer func() { clear(password) }()
	timeout, _ := time.ParseDuration(s.cfg.RequestTimeout)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	transport.enabled.Store(true)
	watchDone := make(chan struct{})
	defer close(watchDone)
	go func() {
		select {
		case <-ctx.Done():
			conn.Close()
		case <-watchDone:
		}
	}()
	conn.SetDeadline(time.Now().Add(timeout))
	go func() {
		for request := range requests {
			request.Reply(request.Type == "keepalive@openssh.com", nil)
		}
	}()
	var sessions sync.WaitGroup
	active := make(chan struct{}, 4)
	for channel := range channels {
		if channel.ChannelType() != "session" {
			channel.Reject(ssh.UnknownChannelType, "only Git sessions are supported")
			continue
		}
		select {
		case active <- struct{}{}:
		default:
			channel.Reject(ssh.ResourceShortage, "session capacity exceeded")
			continue
		}
		ch, requests, err := channel.Accept()
		if err != nil {
			<-active
			continue
		}
		sessions.Add(1)
		go func() {
			defer sessions.Done()
			defer func() { <-active }()
			s.session(ctx, ch, requests, client.User(), password)
		}()
	}
	cancel()
	sessions.Wait()
}

type gitCommand struct {
	name, repo, operation string
}

func unquoteArgument(arg string) string {
	if len(arg) >= 2 && (arg[0] == '\'' || arg[0] == '"') && arg[len(arg)-1] == arg[0] {
		return arg[1 : len(arg)-1]
	}
	return arg
}

func parseCommand(command string) (gitCommand, error) {
	var result gitCommand
	if strings.ContainsAny(command, "\x00\r\n") {
		return result, fmt.Errorf("invalid SSH command")
	}
	name, arg, ok := strings.Cut(command, " ")
	if !ok {
		return result, fmt.Errorf("missing Git repository")
	}
	arg = strings.TrimSpace(arg)
	switch name {
	case "git-upload-pack", "git-receive-pack":
	case "git-lfs-authenticate", "git-lfs-transfer":
		boundary := strings.LastIndexByte(arg, ' ')
		if boundary < 0 {
			return result, fmt.Errorf("missing LFS operation")
		}
		result.operation = unquoteArgument(arg[boundary+1:])
		if result.operation != "download" && result.operation != "upload" {
			return result, fmt.Errorf("unsupported LFS operation")
		}
		arg = strings.TrimSpace(arg[:boundary])
	default:
		return result, fmt.Errorf("unsupported SSH command")
	}
	arg = unquoteArgument(arg)
	// ssh:// URLs send one leading slash; forward a validated relative path to
	// the fixed upstream rather than accepting an absolute filesystem path.
	arg = strings.TrimPrefix(arg, "/")
	if err := config.ValidateRepositoryPath(arg); err != nil {
		return result, err
	}
	result.name, result.repo = name, strings.TrimSuffix(arg, ".git")+".git"
	return result, nil
}

func (s *Server) route(user, repo string) (*origin, error) {
	settings := s.cfg.SSH.Upstream
	if s.cfg.SSH.Mode == config.ModeMulti {
		id, path, ok := strings.Cut(repo, "/")
		if !ok {
			return nil, fmt.Errorf("missing SSH upstream prefix")
		}
		value, ok := s.cfg.SSH.Upstreams[id]
		if !ok {
			return nil, fmt.Errorf("unknown SSH upstream")
		}
		settings = &value
		repo = path
	}
	o, err := makeOrigin(*settings, user, repo, s.git, s.cfg.MaxCacheBytes)
	if err != nil {
		return nil, err
	}
	// A single-origin deployment fronts the same upstream over both
	// protocols: share its scope, keyed by the client-visible repository
	// path. Explicit identities and multi-site routing keep their own keys.
	if s.cfg.SSH.Mode == config.ModeSingle && settings.CacheIdentityURL == "" {
		if identity, ok := s.backend.SharedScopeIdentity(repo); ok {
			o.sharedIdentity = identity
		}
	}
	return o, nil
}

func (s *Server) session(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request, user string, password []byte) {
	defer ch.Close()
	version := ""
	for request := range requests {
		switch request.Type {
		case "env":
			var env struct{ Name, Value string }
			err := ssh.Unmarshal(request.Payload, &env)
			allowed := err == nil && env.Name == "GIT_PROTOCOL" && protocol.IsV2(env.Value)
			if allowed {
				version = env.Value
			}
			request.Reply(allowed, nil)
		case "exec":
			var exec struct{ Command string }
			if ssh.Unmarshal(request.Payload, &exec) != nil {
				request.Reply(false, nil)
				return
			}
			command, err := parseCommand(exec.Command)
			if err != nil {
				request.Reply(false, nil)
				return
			}
			o, err := s.route(user, command.repo)
			if err != nil {
				request.Reply(false, nil)
				return
			}
			request.Reply(true, nil)
			go ssh.DiscardRequests(requests)
			code := s.execute(ctx, ch, o, password, command, version)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			return
		default:
			request.Reply(false, nil)
		}
	}
}

func (s *Server) execute(ctx context.Context, ch ssh.Channel, o *origin, password []byte, command gitCommand, version string) uint32 {
	origin, err := o.open(ctx, password, command, version)
	if err != nil {
		io.WriteString(ch.Stderr(), err.Error()+"\n")
		return 1
	}
	stderrDone := make(chan struct{})
	go func() { io.Copy(ch.Stderr(), origin.err); close(stderrDone) }()
	defer func() { origin.close(); <-stderrDone }()
	if command.name != "git-upload-pack" || !protocol.IsV2(version) {
		return s.proxy(ch, origin, nil, nil)
	}
	advertisement, err := readMessage(origin.out, s.cfg.MaxRefsBytes)
	if err != nil {
		return 1
	}
	if _, err := ch.Write(advertisement); err != nil {
		return 1
	}
	if !bytes.Contains(advertisement, []byte("version 2\n")) {
		return s.proxy(ch, origin, nil, nil)
	}
	for {
		data, err := readBoundedMessage(ch, s.cfg.MaxRequestBytes, protocol.MaxCommandPackets)
		if err == io.EOF {
			return 0
		}
		if err != nil {
			return 1
		}
		fetch, err := protocol.ParseFetch(data)
		if err != nil {
			return 1
		}
		if fetch.Command == "ls-refs" {
			if _, err := origin.in.Write(data); err != nil {
				return 1
			}
			response, err := readMessage(origin.out, s.cfg.MaxRefsBytes)
			if err != nil {
				return 1
			}
			if _, err := ch.Write(response); err != nil {
				return 1
			}
			continue
		}
		if fetch.Command != "fetch" || !fetch.Supported || !fetch.Done || len(fetch.Wants) == 0 {
			return s.proxy(ch, origin, data, nil)
		}
		probeTimeout, _ := time.ParseDuration(s.cfg.UpstreamTimeout)
		probeCtx, probeCancel := context.WithTimeout(ctx, probeTimeout)
		refs, err := o.refs(probeCtx, password, s.cfg.MaxRefsBytes)
		probeCancel()
		if err != nil || !refs.Allows(fetch.Wants) {
			return s.proxy(ch, origin, data, nil)
		}
		file, status, packHit, err := s.backend.PrepareSSH(ctx, o.identity(), fetch, refs, data, cache.HydrateSource(func(ctx context.Context, stage string, roots []string) error {
			s.hydrations.Add(1)
			return o.hydrate(ctx, password, stage, roots)
		}))
		if err != nil {
			return s.proxy(ch, origin, data, nil)
		}
		s.local.Add(1)
		_, _ = status, packHit
		_, err = io.Copy(ch, file)
		file.Close()
		if err != nil {
			return 1
		}
		// Stateful SSH upload-pack exits after a completed fetch. The client
		// waits for this EOF before finishing its pack/index operation.
		return 0
	}
}

func (s *Server) proxy(ch ssh.Channel, source *originSession, input, output []byte) uint32 {
	s.forwarded.Add(1)
	s.backend.RecordSSHPassthrough()
	if len(input) > 0 {
		if _, err := source.in.Write(input); err != nil {
			return 1
		}
	}
	if len(output) > 0 {
		if _, err := ch.Write(output); err != nil {
			return 1
		}
	}
	go func() { io.Copy(source.in, ch); source.in.Close() }()
	if _, err := io.Copy(ch, source.out); err != nil {
		return 1
	}
	err := source.session.Wait()
	if err == nil {
		return 0
	}
	if exit, ok := err.(*ssh.ExitError); ok {
		return uint32(exit.ExitStatus())
	}
	return 1
}
