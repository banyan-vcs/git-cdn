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
	// touch reports progress on the connection to its session clock.
	touch func()
}

func (c *idleConn) Read(data []byte) (int, error) {
	// The connection request deadline bounds reads. A client may legitimately
	// wait silently while the CDN hydrates/produces a local response.
	n, err := c.Conn.Read(data)
	c.progress(n)
	return n, err
}
func (c *idleConn) Write(data []byte) (int, error) {
	if c.enabled.Load() {
		if err := c.Conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
			return 0, err
		}
	}
	n, err := c.Conn.Write(data)
	c.progress(n)
	return n, err
}

func (c *idleConn) progress(n int) {
	if n > 0 && c.touch != nil {
		c.touch()
	}
}
