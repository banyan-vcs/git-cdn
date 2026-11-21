package sshproxy

import (
	"context"
	"log/slog"
	"net"
)

// SessionInfo contains validated exec metadata, never passwords or packet data.
type SessionInfo struct {
	User       string
	Command    string
	Repository string
	Operation  string
}

type Option func(*Server)

// WithLogger selects the logger for handshake diagnostics and session summaries.
func WithLogger(logger *slog.Logger) Option {
	return func(s *Server) {
		if logger != nil {
			s.logger = logger
		}
	}
}

// WithConnContext enriches a connection context before the SSH handshake.
// The callback must derive its result from ctx and be safe for concurrent use.
// Returning nil retains ctx. It must not change conn's transport state.
func WithConnContext(fn func(context.Context, net.Conn) context.Context) Option {
	return func(s *Server) { s.connContext = fn }
}

// WithSessionContext enriches the bounded context of each validated Git exec.
// The callback must derive its result from ctx, preserve cancellation/deadlines,
// and be safe for concurrent use. Returning nil retains ctx.
func WithSessionContext(fn func(context.Context, SessionInfo) context.Context) Option {
	return func(s *Server) { s.sessionContext = fn }
}
