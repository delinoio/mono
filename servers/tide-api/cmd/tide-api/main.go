// SPDX-License-Identifier: FSL-1.1-ALv2

// Command tide-api serves the tide control plane. The owning contract is
// docs/servers-tide-api-foundation.md.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/servers/internal/telemetry"
	"github.com/delinoio/mono/servers/tide-api/internal/app"
	"github.com/delinoio/mono/servers/tide-api/internal/auth"
	"github.com/delinoio/mono/servers/tide-api/internal/config"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/layers"
	"github.com/delinoio/mono/servers/tide-api/internal/ownsandclient"
)

// shutdownTimeout bounds the drain after a termination signal. Server
// streams (dev session exec, WatchQueue) end with the drain; clients resume
// them.
const shutdownTimeout = 10 * time.Second

// HTTP server timeouts. Server streams run for up to 14 minutes, so the
// server sets no ReadTimeout or WriteTimeout: each RPC bounds its own
// lifetime. readHeaderTimeout bounds the request head, idleTimeout an idle
// keep-alive connection and writeByteTimeout an HTTP/2 connection whose peer
// stopped reading.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	writeByteTimeout  = 30 * time.Second
)

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
	tel, err := telemetry.New(context.Background(), telemetry.Options{ServiceName: "tide-api", Level: cfg.LogLevel.SlogLevel()})
	if err != nil {
		return err
	}
	// This deferred call runs last, so the OTLP export flushes the records of
	// the shutdown.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), telemetry.ShutdownTimeout)
		defer cancel()
		if err := tel.Shutdown(ctx); err != nil {
			slog.Error("telemetry_shutdown_failed", slog.Any("error", err))
		}
	}()
	logger := tel.Logger
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	database, err := db.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer database.Close()
	if cfg.RunMigrations {
		if err := database.Migrate(ctx, logger); err != nil {
			return err
		}
	}

	a, err := app.New(app.Options{
		DB:     database,
		Logger: logger,
		Logto: auth.NewLogtoValidator(auth.LogtoConfig{
			Issuer:   cfg.LogtoIssuer,
			Audience: cfg.LogtoAudience,
			JWKSURL:  cfg.LogtoJWKSURL,
			JWKSFile: cfg.LogtoJWKSFile,
			Logger:   logger,
		}),
		Secrets:           cfg.SecretKeys,
		CORSOrigins:       cfg.CORSOrigins,
		ClientAddressMode: cfg.ClientAddressMode,
		Metrics:           tel.MeterProvider,
		Extensions: layers.Extensions(layers.Options{
			OwnSand:      ownsandclient.SDKFactory{BaseURL: cfg.OwnSandAPIOrigin, Logger: logger},
			PublicOrigin: cfg.PublicOrigin,
		}, &layers.Layers{}),
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	var metricsListener net.Listener
	if cfg.MetricsAddr != "" {
		if metricsListener, err = net.Listen("tcp", cfg.MetricsAddr); err != nil {
			_ = listener.Close()
			return err
		}
	}
	srv := newServer(a.Handler(), logger)

	workersCtx, stopWorkers := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(1)
	go func() {
		defer workers.Done()
		a.Run(workersCtx)
	}()
	// stopAndWait releases the lease, which stops the lease components; it
	// runs before the HTTP drain and again on return.
	stopAndWait := func() {
		stopWorkers()
		workers.Wait()
	}
	defer stopAndWait()

	var metricsSrv *http.Server
	metricsAddr := ""
	if metricsListener != nil {
		metricsSrv = newMetricsServer(tel, logger)
		metricsAddr = metricsListener.Addr().String()
		go func() {
			if err := metricsSrv.Serve(metricsListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics_server_failed", slog.Any("error", err))
			}
		}()
	}

	logger.Info("server_started", slog.String("addr", listener.Addr().String()), slog.String("log_level", string(cfg.LogLevel)),
		slog.String("client_address_mode", string(cfg.ClientAddressMode)), slog.String("metrics_addr", metricsAddr),
		slog.String("secret_primary_key_id", cfg.SecretKeys.PrimaryKeyID()),
		slog.Bool("otlp_metrics_export", tel.MetricsExport), slog.Bool("otlp_logs_export", tel.LogsExport))
	err = serve(ctx, stop, srv, listener, logger, shutdownTimeout, stopAndWait)
	if metricsSrv != nil {
		_ = metricsSrv.Close()
	}
	return err
}

// newMetricsServer returns the internal Prometheus listener. It serves only
// GET /metrics and is never mounted on the public handler.
func newMetricsServer(tel *telemetry.Telemetry, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:           tel.MetricsHandler(slog.NewLogLogger(logger.Handler(), slog.LevelError)),
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

// newServer returns the HTTP server. It serves HTTP/1.1 and HTTP/2 without
// TLS (h2c): Caddy terminates TLS and proxies h2c (the `deploy` field sets
// upstreamProtocol h2c), so gRPC clients of the server streams get HTTP/2
// from end to end.
func newServer(handler http.Handler, logger *slog.Logger) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:           handler,
		Protocols:         &protocols,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		HTTP2:             &http.HTTP2Config{WriteByteTimeout: writeByteTimeout},
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

// serve runs srv until Serve fails or ctx ends, then drains it within
// timeout. beforeDrain runs first, so the instance releases the lease and
// stops the lease components before the drain.
func serve(ctx context.Context, stop func(), srv *http.Server, listener net.Listener, logger *slog.Logger, timeout time.Duration, beforeDrain func()) error {
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(listener) }()
	select {
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	stop()
	logger.Info("server_shutdown_started")
	beforeDrain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(shutdownCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		// Streams that did not end within the drain are closed.
		err = srv.Close()
	}
	if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	if err != nil {
		return err
	}
	logger.Info("server_shutdown_completed")
	return nil
}
