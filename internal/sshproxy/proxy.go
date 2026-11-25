package sshproxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// proxyHandshake bounds the exchange with the proxy itself; the tunnel
// that follows inherits the connection's normal deadlines.
const proxyHandshake = 15 * time.Second

// dialOrigin opens the TCP connection to the origin, directly or through
// the configured outbound proxy. HTTP proxies are crossed with CONNECT and
// SOCKS5 proxies with a username/password-capable handshake; both leave a
// plain byte stream, which is what the SSH transport needs.
func dialOrigin(ctx context.Context, proxy *url.URL, address string) (net.Conn, error) {
	dialer := &net.Dialer{Timeout: proxyHandshake}
	if proxy == nil {
		return dialer.DialContext(ctx, "tcp", address)
	}
	conn, err := dialer.DialContext(ctx, "tcp", proxy.Host)
	if err != nil {
		return nil, err
	}
	tunnel, err := func() (net.Conn, error) {
		if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) > proxyHandshake {
			_ = conn.SetDeadline(time.Now().Add(proxyHandshake))
		}
		if proxy.Scheme == "http" || proxy.Scheme == "https" {
			return connectTunnel(ctx, conn, proxy, address)
		}
		return socks5Tunnel(conn, proxy, address)
	}()
	if err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return tunnel, nil
}

// connectTunnel crosses an HTTP proxy with CONNECT. An https proxy is
// upgraded to TLS first; credentials travel in Proxy-Authorization.
func connectTunnel(ctx context.Context, conn net.Conn, proxy *url.URL, address string) (net.Conn, error) {
	if proxy.Scheme == "https" {
		// The certificate presented by the proxy is verified against the
		// system roots, as any other TLS connection.
		secure := tls.Client(conn, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
		if err := secure.HandshakeContext(ctx); err != nil {
			return nil, err
		}
		conn = secure
	}
	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: address},
		Host:   address,
		Header: make(http.Header),
	}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		request.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(proxy.User.Username()+":"+password)))
	}
	// Request.Write emits the request line and headers only; a CONNECT
	// request has no body.
	if err := request.Write(conn); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, err
	}
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("proxy refused the tunnel with status %d", response.StatusCode)
	}
	// Anything the proxy sent beyond its headers belongs to the tunnel.
	if reader.Buffered() > 0 {
		conn = &bufferedConn{Conn: conn, reader: reader}
	}
	return conn, nil
}

// socks5Tunnel performs the SOCKS5 greeting, optional username/password
// authentication (RFC 1929) and a CONNECT to a domain-name target, so the
// proxy resolves the origin's name.
func socks5Tunnel(conn net.Conn, proxy *url.URL, address string) (net.Conn, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid origin address")
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("invalid origin port")
	}
	methods := []byte{0x00}
	if proxy.User != nil {
		methods = append(methods, 0x02)
	}
	if _, err := conn.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return nil, err
	}
	var reply [2]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, err
	}
	if reply[0] != 0x05 {
		return nil, errors.New("proxy is not SOCKS5")
	}
	switch reply[1] {
	case 0x00:
	case 0x02:
		password, _ := proxy.User.Password()
		credentials := append([]byte{0x01, byte(len(proxy.User.Username()))}, proxy.User.Username()...)
		credentials = append(credentials, byte(len(password)))
		credentials = append(credentials, password...)
		if _, err := conn.Write(credentials); err != nil {
			return nil, err
		}
		if _, err := io.ReadFull(conn, reply[:]); err != nil {
			return nil, err
		}
		if reply[0] != 0x01 || reply[1] != 0x00 {
			return nil, errors.New("proxy rejected the credentials")
		}
	default:
		return nil, errors.New("proxy accepts no supported authentication")
	}
	request := []byte{0x05, 0x01, 0x00, 0x03, byte(len(host))}
	request = append(request, host...)
	var portBytes [2]byte
	binary.BigEndian.PutUint16(portBytes[:], uint16(port))
	request = append(request, portBytes[:]...)
	if _, err := conn.Write(request); err != nil {
		return nil, err
	}
	var head [4]byte
	if _, err := io.ReadFull(conn, head[:]); err != nil {
		return nil, err
	}
	if head[1] != 0x00 {
		return nil, fmt.Errorf("proxy refused the connection with status %d", head[1])
	}
	// The reply ends with the bound address, which is of no use here; it
	// must still be consumed so the tunnel starts at the right byte.
	var length int
	switch head[3] {
	case 0x01:
		length = 4
	case 0x03:
		if _, err := io.ReadFull(conn, reply[:1]); err != nil {
			return nil, err
		}
		length = int(reply[0])
	case 0x04:
		length = 16
	default:
		return nil, errors.New("invalid proxy reply")
	}
	if _, err := io.CopyN(io.Discard, conn, int64(length+2)); err != nil {
		return nil, err
	}
	return conn, nil
}

// bufferedConn replays bytes a bufio reader took off the connection before
// its caller saw them.
type bufferedConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *bufferedConn) Read(p []byte) (int, error) { return c.reader.Read(p) }
