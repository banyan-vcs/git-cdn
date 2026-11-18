package sshproxy

import (
	"net"
	"sync/atomic"
	"time"
)

type idleConn struct {
	net.Conn
	timeout time.Duration
	enabled atomic.Bool
}

func (c *idleConn) Read(data []byte) (int, error) {
	// The connection request deadline bounds reads. A client may legitimately
	// wait silently while the CDN hydrates/produces a local response.
	return c.Conn.Read(data)
}
func (c *idleConn) Write(data []byte) (int, error) {
	if c.enabled.Load() {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	return c.Conn.Write(data)
}
