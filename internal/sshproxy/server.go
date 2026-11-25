package sshproxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/cache"
	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/logging"
	"gitlab.com/banyan-vcs/git-cdn/internal/nativegit"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
	"gitlab.com/banyan-vcs/git-cdn/internal/server"
	"golang.org/x/crypto/ssh"
)

const passwordOnlyBanner = "Git CDN supports SSH password/token authentication only; public-key authentication is not supported.\n" +
	"Use a password/token accepted by the origin, or connect directly to the origin to use SSH keys.\n"

type Server struct {
	cfg              config.Config
	backend          *server.Server
	git              *nativegit.Runner
	signer           ssh.Signer
	slots            chan struct{}
	mu               sync.Mutex
	connections      map[net.Conn]bool
	closed           bool
	workers          sync.WaitGroup
	local, forwarded atomic.Uint64
	hydrations       atomic.Uint64
	logger           *slog.Logger
	connContext      func(context.Context, net.Conn) context.Context
	sessionContext   func(context.Context, SessionInfo) context.Context
	probes           probeFlights
	authFailures     authThrottle
	outboundProxy    *url.URL
	maxRefs          int64
}

func New(c config.Config, backend *server.Server, options ...Option) (*Server, error) {
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
	s := &Server{cfg: c, backend: backend, git: g, signer: signer, slots: make(chan struct{}, c.SSH.MaxConnections), connections: map[net.Conn]bool{}, logger: slog.Default(), maxRefs: c.MaxRefsBytes}
	if c.UpstreamProxy != "" {
		// Validated by the configuration loader.
		s.outboundProxy, _ = url.Parse(c.UpstreamProxy)
	}
	for _, option := range options {
		option(s)
	}
	return s, nil
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
		// A source address blocked for repeated password failures is
		// closed before the handshake: it never reaches the origin.
		if peer, _, err := net.SplitHostPort(conn.RemoteAddr().String()); err == nil && s.authFailures.blocked(peer) {
			s.logger.Warn("SSH connection refused by authentication throttle", "remote_ip", peer)
			conn.Close()
			continue
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

// Drain waits up to grace for existing connections to finish on their own
// and then closes whatever remains. The listener must already be closed,
// so nothing new arrives while it waits. Without it a stop signal cut every
// running clone at once while HTTP responses still had their own window.
func (s *Server) Drain(grace time.Duration) {
	done := make(chan struct{})
	go func() {
		s.workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(grace):
	}
	s.Close()
}

// errIdleSession cancels a connection that stayed silent for longer than
// the request timeout.
var errIdleSession = errors.New("SSH session idle beyond the request timeout")

// sessionClock bounds an SSH connection by silence instead of by age. The
// connection deadline is lastActivity + timeout: every byte read or written
// restarts it, and so does a tick on which the connection is still active.
// While local work runs on the session's behalf — hydration and packing,
// which produce no traffic — the clock is suspended; that work has its own
// deadlines. A clone that keeps transferring is therefore never cut at a
// fixed age, which for a large repository on a slow link fell inside the
// transfer, and a client that goes silent is still disconnected after the
// timeout.
type sessionClock struct {
	conn    net.Conn
	timeout time.Duration
	// ready arms the clock after the handshake, which has its own short
	// bound that activity must not extend.
	ready atomic.Bool
	// busy suspends the clock while local work runs for the session.
	busy atomic.Bool
	mu   sync.Mutex
	last time.Time
	// extended is when the connection deadline was last installed.
	extended time.Time
}

func (c *sessionClock) touch() {
	if !c.ready.Load() {
		return
	}
	c.mu.Lock()
	c.last = time.Now()
	c.mu.Unlock()
	c.extend()
}

// extend moves the connection deadline to the idle horizon. It is called on
// activity and periodically, so a transfer keeps its deadline ahead of it
// even when no single read or write reports progress.
func (c *sessionClock) extend() {
	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	// Throttle the syscall: reinstalling once a quarter of the horizon
	// has elapsed keeps the deadline at least three quarters away.
	if !c.extended.IsZero() && now.Before(c.extended.Add(c.timeout/4)) {
		return
	}
	c.extended = now
	_ = c.conn.SetDeadline(now.Add(c.timeout))
}

// idle reports how long the connection has been silent. Local work counts
// as activity: a session producing a local response is not idle.
func (c *sessionClock) idle(now time.Time) time.Duration {
	if c.busy.Load() {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return now.Sub(c.last)
}

func (c *sessionClock) watch(timeout time.Duration, done <-chan struct{}, cancel context.CancelCauseFunc) {
	ticker := time.NewTicker(max(timeout/4, 50*time.Millisecond))
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case now := <-ticker.C:
			if idle := c.idle(now); idle >= timeout {
				cancel(errIdleSession)
				return
			}
			c.extend()
		}
	}
}

// idleContext is the session context of a connection bounded by silence.
// Cancellation and values come from the embedded context; its deadline is
// answered from the clock, so it reports when the session would time out if
// it stayed silent rather than a fixed moment.
type idleContext struct {
	context.Context
	clock *sessionClock
}

func (c *idleContext) Deadline() (time.Time, bool) {
	c.clock.mu.Lock()
	last := c.clock.last
	c.clock.mu.Unlock()
	if c.clock.busy.Load() {
		last = time.Now()
	}
	return last.Add(c.clock.timeout), true
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
	base := context.Background()
	if s.connContext != nil {
		if enriched := s.connContext(base, conn); enriched != nil {
			base = enriched
		}
	}
	peer, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	idleTimeout, _ := time.ParseDuration(s.cfg.WriteIdleTimeout)
	clock := &sessionClock{conn: conn}
	transport := &idleConn{Conn: conn, timeout: idleTimeout, touch: clock.touch}
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
		s.logger.DebugContext(base, "SSH handshake failed", "reason", "handshake")
		return
	}
	defer client.Close()
	defer func() { clear(password) }()
	timeout, _ := time.ParseDuration(s.cfg.RequestTimeout)
	clock.timeout = timeout
	bound, cancel := context.WithCancelCause(base)
	// The session context keeps a Deadline: the idle horizon, which moves
	// with activity. Embedders deriving their own timeouts from it see the
	// moment the session may be canceled if nothing happens.
	ctx := &idleContext{Context: bound, clock: clock}
	defer cancel(nil)
	transport.enabled.Store(true)
	watchDone := make(chan struct{})
	defer close(watchDone)
	go clock.watch(timeout, watchDone, cancel)
	go func() {
		<-ctx.Done()
		conn.Close()
	}()
	clock.mu.Lock()
	clock.last = time.Now()
	clock.mu.Unlock()
	clock.ready.Store(true)
	clock.extend()
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
			s.session(ctx, ch, requests, client.User(), password, sessionScope{peer: peer, clock: clock})
		}()
	}
	cancel(nil)
	sessions.Wait()
}

// sessionScope carries what an exec needs from its connection: the source
// address for the authentication throttle and the activity clock.
type sessionScope struct {
	peer  string
	clock *sessionClock
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
	o, err := makeOrigin(*settings, user, repo, s.git, s.cfg.MaxCacheBytes, s.outboundProxy)
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

func (s *Server) session(ctx context.Context, ch ssh.Channel, requests <-chan *ssh.Request, user string, password []byte, scope sessionScope) {
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
			if s.sessionContext != nil {
				info := SessionInfo{User: user, Command: command.name, Repository: command.repo, Operation: command.operation}
				if enriched := s.sessionContext(ctx, info); enriched != nil {
					ctx = enriched
				}
			}
			o, err := s.route(user, command.repo)
			if err != nil {
				request.Reply(false, nil)
				return
			}
			request.Reply(true, nil)
			go ssh.DiscardRequests(requests)
			code := s.execute(ctx, ch, o, password, command, version, scope)
			ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{code}))
			return
		default:
			request.Reply(false, nil)
		}
	}
}

func (s *Server) execute(ctx context.Context, ch ssh.Channel, o *origin, password []byte, command gitCommand, version string, scope sessionScope) (code uint32) {
	ctx = logging.WithLogger(ctx, s.logger.With("transport", "ssh", "repository", command.repo, "command", command.name, "upstream_host", o.settings.Address))
	started := time.Now()
	output := &countingChannel{Channel: ch}
	ch = output
	cacheStatus := "NONE"
	packCacheHit := false
	defer func() {
		level := slog.LevelInfo
		if code != 0 {
			level = slog.LevelWarn
		}
		if ctx.Err() != nil {
			level = slog.LevelDebug
		}
		logging.Logger(ctx, s.logger).Log(ctx, level, "SSH session completed", "upstream_user", o.user,
			"cache_status", cacheStatus, "pack_cache_hit", packCacheHit, "exit_status", code, "duration_ms", time.Since(started).Milliseconds(), "response_bytes", output.bytes)
	}()
	origin, err := o.open(ctx, password, command, version)
	if err != nil {
		// Only the origin judges passwords; a rejection there is the
		// brute-force signal this proxy can see.
		var failure *originError
		if scope.peer != "" && errors.As(err, &failure) && failure.reason == "ssh_authentication" {
			s.authFailures.record(scope.peer)
		}
		io.WriteString(ch.Stderr(), err.Error()+"\n")
		return 1
	}
	if scope.peer != "" {
		s.authFailures.accepted(scope.peer)
	}
	stderrDone := make(chan struct{})
	go func() { io.Copy(ch.Stderr(), origin.err); close(stderrDone) }()
	defer func() { origin.close(); <-stderrDone }()
	passthrough := func(input []byte) uint32 {
		cacheStatus = "BYPASS"
		return s.proxy(ch, origin, input, nil)
	}
	if command.name != "git-upload-pack" || !protocol.IsV2(version) {
		return passthrough(nil)
	}
	advertisement, err := readMessage(origin.out, s.cfg.MaxRefsBytes)
	if err != nil {
		return 1
	}
	if _, err := ch.Write(advertisement); err != nil {
		return 1
	}
	if !bytes.Contains(advertisement, []byte("version 2\n")) {
		return passthrough(nil)
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
			return passthrough(data)
		}
		probeTimeout, _ := time.ParseDuration(s.cfg.UpstreamTimeout)
		refs, err := s.sharedRefs(ctx, o, password, probeTimeout)
		if err != nil || !refs.Allows(fetch.Wants) {
			if err == nil {
				logging.Logger(ctx, s.logger).InfoContext(ctx, "SSH fetch forwarded", "reason", "unadvertised_oid")
			}
			return passthrough(data)
		}
		// Hydration and packing produce no traffic while they run; the
		// connection clock stands still for them and they answer to their
		// own deadlines.
		if scope.clock != nil {
			scope.clock.busy.Store(true)
		}
		file, status, packHit, err := s.backend.PrepareSSH(ctx, o.identity(), fetch, refs, data, cache.HydrateSource(func(ctx context.Context, stage string, roots []string) error {
			s.hydrations.Add(1)
			return o.hydrate(ctx, password, stage, roots)
		}))
		if scope.clock != nil {
			scope.clock.busy.Store(false)
			scope.clock.touch()
		}
		if err != nil {
			return passthrough(data)
		}
		s.local.Add(1)
		cacheStatus = status
		packCacheHit = packHit
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

type countingChannel struct {
	ssh.Channel
	bytes int64
}

func (c *countingChannel) Write(p []byte) (int, error) {
	n, err := c.Channel.Write(p)
	c.bytes += int64(n)
	return n, err
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
