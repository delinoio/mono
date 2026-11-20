// SPDX-License-Identifier: FSL-1.1-ALv2

// Command ownsand-api serves the OwnSand control plane.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/jobs"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/server"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
	"github.com/delinoio/mono/servers/ownsand-api/internal/stunserver"
)

const shutdownTimeout = 10 * time.Second

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("server_exited", slog.Any("error", err))
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel.SlogLevel()}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := store.Migrate(ctx, st.Pool, logger); err != nil {
		return err
	}

	a := app.New(cfg, st, logger, app.Options{})
	bgCtx, cancelBg := context.WithCancel(context.Background())
	defer cancelBg()

	leases := &lease.Manager{Backend: st, Holder: datapath.NewID(), Logger: logger}
	a.UseLease(leases)
	go leases.Run(bgCtx)
	go (&jobs.Runner{Store: st, Logger: logger, Objects: a.ObjectStore()}).Run(bgCtx)
	a.Start(bgCtx)

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	udp, err := net.ListenPacket("udp", cfg.Extra.STUNAddr)
	if err != nil {
		_ = listener.Close()
		return err
	}
	go func() {
		if err := stunserver.New(udp, logger).Serve(bgCtx); err != nil {
			logger.Error("stun_server_failed", slog.Any("error", err))
		}
	}()
	srv := server.New(a.Handler, logger)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	logger.Info("server_started", slog.String("addr", listener.Addr().String()), slog.String("stun_addr", udp.LocalAddr().String()), slog.String("log_level", string(cfg.LogLevel)))

	select {
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	stop()
	logger.Info("server_shutdown_started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	// Release the lease first and close every agent channel and agent relay
	// connection, so that agents reconnect to the new instance at once.
	leases.Release(shutdownCtx)
	a.Drain(shutdownCtx)
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancelBg()
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		shutdownErr = errors.Join(shutdownErr, err)
	}
	if shutdownErr != nil {
		return shutdownErr
	}
	logger.Info("server_shutdown_completed")
	return nil
}
