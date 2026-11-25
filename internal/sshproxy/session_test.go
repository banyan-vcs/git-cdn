package sshproxy

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"gitlab.com/banyan-vcs/git-cdn/internal/protocol"
)

// The session clock bounds a connection by silence: activity restarts it,
// local work suspends it, and silence for the timeout cancels the session.
func TestSessionClockFollowsActivity(t *testing.T) {
	origin, client := net.Pipe()
	defer origin.Close()
	defer client.Close()
	clock := &sessionClock{conn: client, timeout: 150 * time.Millisecond}
	transport := &idleConn{Conn: client, touch: clock.touch}
	bound, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	ctx := &idleContext{Context: bound, clock: clock}
	done := make(chan struct{})
	defer close(done)
	go clock.watch(clock.timeout, done, cancel)

	clock.mu.Lock()
	clock.last = time.Now()
	clock.mu.Unlock()
	clock.ready.Store(true)
	clock.extend()

	// A silent connection is read-closed at the horizon.
	started := time.Now()
	_, err := transport.Read(make([]byte, 1))
	require.Error(t, err, "a silent connection was not bounded")
	elapsed := time.Since(started)
	require.GreaterOrEqual(t, elapsed, 100*time.Millisecond)
	require.Less(t, elapsed, 3*time.Second, "the idle horizon was not applied")

	// Bytes from the client restart the clock, moving the horizon and the
	// deadline the session context reports.
	first, ok := ctx.Deadline()
	require.True(t, ok, "the session context lost its deadline")
	go func() {
		origin.Write([]byte("progress"))
		origin.Close()
	}()
	buffer := make([]byte, len("progress"))
	client.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, err = io.ReadFull(transport, buffer)
	require.NoError(t, err)
	require.Equal(t, "progress", string(buffer))
	second, ok := ctx.Deadline()
	require.True(t, ok)
	require.True(t, second.After(first), "progress did not move the deadline")

	// Local work is not silence.
	clock.busy.Store(true)
	require.Zero(t, clock.idle(time.Now().Add(time.Hour)))
	clock.busy.Store(false)

	// Silence for longer than the timeout ends the session with its cause.
	select {
	case <-ctx.Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "a silent session was never canceled")
	}
	require.ErrorIs(t, context.Cause(ctx), errIdleSession)
}

// Concurrent ref lookups for one repository and password share a single
// origin connection, and each caller receives the leader's fresh snapshot.
func TestConcurrentRefLookupsShareOneOriginConnection(t *testing.T) {
	f := newSSHFixture(t)
	f.delay.Store(int64(400 * time.Millisecond))
	o, err := f.server.route("git", "Group/Repo.git")
	require.NoError(t, err)
	before := f.passwordAttempts.Load()
	const callers = 6
	results := make([]protocol.Snapshot, callers)
	failures := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i], failures[i] = f.server.sharedRefs(context.Background(), o, []byte("fixture-token"), 10*time.Second)
		}()
	}
	close(start)
	wg.Wait()
	for i := range callers {
		require.NoError(t, failures[i])
		require.NotEmpty(t, results[i].Refs)
		require.Equal(t, results[0].Fingerprint(), results[i].Fingerprint(), "a waiter received a different snapshot")
	}
	require.Equal(t, before+1, f.passwordAttempts.Load(), "merged lookups opened more than one origin connection")

	// A different password is a different lookup: credentials never share.
	_, err = f.server.sharedRefs(context.Background(), o, []byte("another-token"), 10*time.Second)
	require.Error(t, err, "the origin accepted a password it does not know")
	require.Equal(t, before+2, f.passwordAttempts.Load())
}

// Only the origin judges passwords. A run of rejections from one source
// address is closed before the origin sees it, a success forgets the
// address, and a block outlives attempts made during it.
func TestAuthenticationThrottle(t *testing.T) {
	var throttle authThrottle
	require.False(t, throttle.blocked("10.0.0.1"))
	for range authFailureLimit - 1 {
		throttle.record("10.0.0.1")
	}
	require.False(t, throttle.blocked("10.0.0.1"), "fewer than the limit blocked the address")
	throttle.record("10.0.0.1")
	require.True(t, throttle.blocked("10.0.0.1"), "the limit did not block the address")
	require.False(t, throttle.blocked("10.0.0.2"), "another address was blocked")

	throttle.accepted("10.0.0.1")
	require.False(t, throttle.blocked("10.0.0.1"), "a successful authentication did not clear the failures")
	for range authFailureLimit {
		throttle.record("10.0.0.1")
	}
	throttle.mu.Lock()
	blockedUntil := throttle.failures["10.0.0.1"].blockedUntil
	throttle.mu.Unlock()
	// An address that keeps trying during its block stays out.
	time.Sleep(10 * time.Millisecond)
	require.True(t, throttle.blocked("10.0.0.1"))
	throttle.mu.Lock()
	extended := throttle.failures["10.0.0.1"].blockedUntil
	throttle.mu.Unlock()
	require.True(t, extended.After(blockedUntil), "a connection during the block did not restart it")

	// Failures spread over more than the window do not add up.
	throttle = authThrottle{}
	old := time.Now().Add(-2 * authFailureWindow)
	throttle.mu.Lock()
	throttle.failures = map[string]*authFailures{"10.0.0.3": {count: authFailureLimit - 1, window: old}}
	throttle.mu.Unlock()
	throttle.record("10.0.0.3")
	require.False(t, throttle.blocked("10.0.0.3"), "stale failures counted towards the limit")
}

// Drain waits for running sessions up to the grace period and closes what
// is left when it passes.
func TestDrainWaitsThenCloses(t *testing.T) {
	s := &Server{logger: slog.Default(), connections: map[net.Conn]bool{}}
	_, session := net.Pipe()
	s.connections[session] = true
	released := make(chan struct{})
	s.workers.Add(1)
	go func() {
		defer s.workers.Done()
		defer close(released)
		session.Read(make([]byte, 1)) // returns once the connection closes
	}()
	started := time.Now()
	s.Drain(40 * time.Millisecond)
	require.GreaterOrEqual(t, time.Since(started), 40*time.Millisecond, "drain did not wait for the grace period")
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test failed", "a session outliving the grace period was not closed")
	}
}

// echoOrigin is an origin that returns every byte it receives.
func echoOrigin(t *testing.T) net.Addr {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				io.Copy(conn, conn)
			}()
		}
	}()
	return listener.Addr()
}

func roundTrip(t *testing.T, conn net.Conn) {
	t.Helper()
	defer conn.Close()
	message := "git-upload-pack"
	go conn.Write([]byte(message))
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	buffer := make([]byte, len(message))
	_, err := io.ReadFull(conn, buffer)
	require.NoError(t, err)
	require.Equal(t, message, string(buffer))
}

func TestDialOriginThroughHTTPConnectProxy(t *testing.T) {
	target := echoOrigin(t)
	var requests atomic.Int32
	var authorization atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodConnect {
			http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
			return
		}
		authorization.Store(r.Header.Get("Proxy-Authorization"))
		upstream, err := net.Dial("tcp", r.Host)
		if err != nil {
			http.Error(w, "unreachable", http.StatusBadGateway)
			return
		}
		client, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			upstream.Close()
			return
		}
		defer client.Close()
		client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
		go func() {
			io.Copy(upstream, client)
			upstream.Close()
		}()
		io.Copy(client, upstream)
	}))
	defer proxy.Close()
	settings, err := url.Parse(proxy.URL)
	require.NoError(t, err)
	settings.User = url.UserPassword("gitcdn", "secret")
	conn, err := dialOrigin(context.Background(), settings, target.String())
	require.NoError(t, err)
	roundTrip(t, conn)
	require.EqualValues(t, 1, requests.Load())
	require.Equal(t, "Basic "+base64.StdEncoding.EncodeToString([]byte("gitcdn:secret")), authorization.Load())

	// A proxy that refuses the tunnel is an error, not a half-open tunnel.
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer refusing.Close()
	settings, _ = url.Parse(refusing.URL)
	_, err = dialOrigin(context.Background(), settings, target.String())
	require.Error(t, err)

	// Without a proxy the origin is dialed directly.
	conn, err = dialOrigin(context.Background(), nil, target.String())
	require.NoError(t, err)
	roundTrip(t, conn)
	require.EqualValues(t, 1, requests.Load(), "a direct dial went through the proxy")
}

// socks5Origin is a SOCKS5 proxy that optionally demands username and
// password; it records the host it was asked to reach.
func socks5Origin(t *testing.T, user, password string) (*url.URL, *atomic.Value) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { listener.Close() })
	var reached atomic.Value
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				head := make([]byte, 2)
				if _, err := io.ReadFull(client, head); err != nil {
					return
				}
				methods := make([]byte, head[1])
				if _, err := io.ReadFull(client, methods); err != nil {
					return
				}
				method := byte(0x00)
				if user != "" {
					method = 0x02
				}
				client.Write([]byte{0x05, method})
				if method == 0x02 {
					length := make([]byte, 2)
					if _, err := io.ReadFull(client, length); err != nil {
						return
					}
					name := make([]byte, length[1])
					if _, err := io.ReadFull(client, name); err != nil {
						return
					}
					if _, err := io.ReadFull(client, length[:1]); err != nil {
						return
					}
					secret := make([]byte, length[0])
					if _, err := io.ReadFull(client, secret); err != nil {
						return
					}
					if string(name) != user || string(secret) != password {
						client.Write([]byte{0x01, 0x01})
						return
					}
					client.Write([]byte{0x01, 0x00})
				}
				request := make([]byte, 4)
				if _, err := io.ReadFull(client, request); err != nil {
					return
				}
				if request[3] != 0x03 {
					client.Write([]byte{0x05, 0x08, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				length := make([]byte, 1)
				if _, err := io.ReadFull(client, length); err != nil {
					return
				}
				host := make([]byte, length[0])
				if _, err := io.ReadFull(client, host); err != nil {
					return
				}
				port := make([]byte, 2)
				if _, err := io.ReadFull(client, port); err != nil {
					return
				}
				address := net.JoinHostPort(string(host), strconv.Itoa(int(binary.BigEndian.Uint16(port))))
				reached.Store(address)
				upstream, err := net.Dial("tcp", address)
				if err != nil {
					client.Write([]byte{0x05, 0x05, 0x00, 0x01, 0, 0, 0, 0, 0, 0})
					return
				}
				defer upstream.Close()
				client.Write([]byte{0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0})
				go func() {
					io.Copy(upstream, client)
					upstream.Close()
				}()
				io.Copy(client, upstream)
			}()
		}
	}()
	proxy := &url.URL{Scheme: "socks5", Host: listener.Addr().String()}
	if user != "" {
		proxy.User = url.UserPassword(user, password)
	}
	return proxy, &reached
}

func TestDialOriginThroughSOCKS5Proxy(t *testing.T) {
	target := echoOrigin(t)
	for _, test := range []struct {
		name, user, password string
	}{{"without credentials", "", ""}, {"with credentials", "gitcdn", "secret"}} {
		t.Run(test.name, func(t *testing.T) {
			proxy, reached := socks5Origin(t, test.user, test.password)
			conn, err := dialOrigin(context.Background(), proxy, target.String())
			require.NoError(t, err)
			roundTrip(t, conn)
			require.Equal(t, target.String(), reached.Load(), "the proxy was not asked for the origin")
		})
	}
	// A rejected password does not become a tunnel.
	proxy, _ := socks5Origin(t, "gitcdn", "secret")
	proxy.User = url.UserPassword("gitcdn", "wrong")
	conn, err := dialOrigin(context.Background(), proxy, target.String())
	require.Error(t, err, "the proxy accepted a wrong password")
	require.Nil(t, conn)
}
