package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gitlab.com/banyan-vcs/git-cdn/internal/config"
	"gitlab.com/banyan-vcs/git-cdn/internal/server"
	"gitlab.com/banyan-vcs/git-cdn/internal/sshproxy"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(logger); err != nil {
		logger.Error("git CDN failed", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	path := flag.String("config", config.DefaultConfigPath, "Path to the YAML configuration")
	flag.Parse()
	c, err := config.Load(*path)
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
		sshServer, err := sshproxy.New(c, handler)
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
		go func() { <-ctx.Done(); sshListener.Close(); sshServer.Close() }()
		go func() {
			if err := sshServer.Serve(sshListener); err != nil && !errors.Is(err, net.ErrClosed) {
				logger.Error("SSH listener failed")
				stop()
			}
		}()
		logger.Info("git CDN SSH listening", "address", sshListener.Addr().String())
	}
	httpServer := &http.Server{Addr: c.Listen, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 5 * time.Minute, IdleTimeout: 2 * time.Minute, BaseContext: func(_ net.Listener) context.Context { return ctx }}
	listener, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	shutdownDone := make(chan struct{})
	go func() {
		defer close(shutdownDone)
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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
