package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/server"
	"gitlab.com/banyan-vcs/git-cdn/internal/sshproxy"
)

// logLevel adapts slog.Level to the pflag Value interface, parsing the
// same DEBUG/INFO/WARN/ERROR words the flag has always accepted.
type logLevel slog.Level

func (l *logLevel) String() string { return slog.Level(*l).String() }

func (l *logLevel) Type() string { return "level" }

func (l *logLevel) Set(text string) error {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(text)); err != nil {
		return fmt.Errorf("invalid log level %q", text)
	}
	*l = logLevel(parsed)
	return nil
}

func newServeCommand() *cobra.Command {
	var (
		path  string
		level logLevel = logLevel(slog.LevelInfo)
	)
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the CDN daemon",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: (*slog.Level)(&level)}))
			return serve(logger, path)
		},
	}
	cmd.Flags().StringVar(&path, "config", config.DefaultConfigPath, "Path to the YAML configuration")
	cmd.Flags().Var(&level, "log-level", "Log level: DEBUG, INFO, WARN or ERROR")
	return cmd
}

// shutdownGrace bounds how long in-flight work may finish after a stop
// signal. HTTP responses and SSH sessions get the same window.
const shutdownGrace = 15 * time.Second

func serve(logger *slog.Logger, path string) error {
	c, err := config.Load(path)
	if err != nil {
		return err
	}
	handler, err := server.New(c, logger)
	if err != nil {
		return err
	}
	defer handler.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if c.SSH != nil {
		sshServer, err := sshproxy.New(c, handler, sshproxy.WithLogger(logger))
		if err != nil {
			return err
		}
		sshListener, err := net.Listen("tcp", c.SSH.Listen)
		if err != nil {
			sshServer.Close()
			return err
		}
		defer sshServer.Close()
		defer sshListener.Close()
		go func() {
			<-ctx.Done()
			// Stop accepting first, then give running clones the same
			// grace the HTTP server gets before they are cut off.
			sshListener.Close()
			sshServer.Drain(shutdownGrace)
		}()
		go func() {
			if err := sshServer.Serve(sshListener); err != nil && !errors.Is(err, net.ErrClosed) {
				logger.Error("SSH listener failed")
				stop()
			}
		}()
		logger.Info("git CDN SSH listening", "address", sshListener.Addr().String())
	}
	// No absolute ReadTimeout: it bounded the whole request including a
	// push body, so an upload that took longer than it was cut off while
	// still making progress. Headers keep their own deadline, and the
	// handler bounds each request-body read by the idle timeout instead.
	httpServer := &http.Server{Addr: c.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, BaseContext: func(_ net.Listener) context.Context { return ctx }}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
		defer cancel()
		if err := httpServer.Shutdown(shutdown); err != nil {
			httpServer.Close()
		}
	}()
	logger.Info("git CDN listening", "address", listener.Addr().String(), "mode", c.Mode, "tls", c.TLSCert != "")
	if c.TLSCert != "" {
		err = httpServer.ServeTLS(listener, c.TLSCert, c.TLSKey)
	} else {
		err = httpServer.Serve(listener)
	}
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		stop()
	}
	stop()
	<-shutdownDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
