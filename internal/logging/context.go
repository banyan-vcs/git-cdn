// Package logging provides context correlation and credential-safe error fields.
package logging

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"syscall"
)

type loggerKey struct{}

func WithLogger(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey{}, logger)
}

func Logger(ctx context.Context, fallback *slog.Logger) *slog.Logger {
	if ctx != nil {
		if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok && logger != nil {
			return logger
		}
	}
	if fallback != nil {
		return fallback
	}
	return slog.Default()
}

// ErrorAttrs never formats arbitrary errors: URLs, command arguments, paths and
// upstream text may contain credentials. Known error structures retain useful
// classes, operations, system errors and exit codes without their input data.
func ErrorAttrs(err error) []slog.Attr {
	if err == nil {
		return nil
	}
	kind := "operation_failed"
	switch {
	case errors.Is(err, context.Canceled):
		kind = "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		kind = "deadline_exceeded"
	case errors.Is(err, fs.ErrPermission):
		kind = "permission_denied"
	case errors.Is(err, fs.ErrNotExist):
		kind = "not_found"
	case errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, io.EOF):
		kind = "unexpected_eof"
	}
	attrs := []slog.Attr{slog.String("error_kind", kind)}
	var coded interface{ LogKind() string }
	if errors.As(err, &coded) {
		attrs = append(attrs, slog.String("error_reason", coded.LogKind()))
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		attrs = append(attrs, slog.Int("exit_code", exit.ExitCode()))
	}
	var network *net.OpError
	if errors.As(err, &network) {
		attrs = append(attrs, slog.String("network_op", network.Op), slog.String("network", network.Net))
	}
	var path *os.PathError
	if errors.As(err, &path) {
		attrs = append(attrs, slog.String("filesystem_op", path.Op))
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		attrs = append(attrs, slog.Int("errno", int(errno)), slog.String("system_error", errno.Error()))
	}
	return attrs
}

func Failure(ctx context.Context, fallback *slog.Logger, message string, err error, attrs ...slog.Attr) {
	level := slog.LevelWarn
	if errors.Is(err, context.Canceled) {
		level = slog.LevelDebug
	}
	attrs = append(attrs, ErrorAttrs(err)...)
	Logger(ctx, fallback).LogAttrs(ctx, level, message, attrs...)
}
