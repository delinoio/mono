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
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/internal/telemetry"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
	"github.com/delinoio/mono/servers/ownsand-api/internal/stun"
)

// shutdownTimeout bounds the drain after a termination signal. Long-lived
// streams (the agent channel and relay connections) end with the drain.
const shutdownTimeout = 10 * time.Second

// HTTP server timeouts. The server carries long-lived bidirectional streams
// (the agent channel and RelayAgentService.Connect) and relay server
// streams of up to 14 minutes, so it sets no ReadTimeout or WriteTimeout:
// each RPC bounds its own lifetime (Connect deadlines, the relay limits and
// the heartbeat timeout). readHeaderTimeout bounds the request head and
// idleTimeout an idle keep-alive connection. writeByteTimeout closes an
// HTTP/2 connection that accepts no byte for that long while data waits to
// be written: a relay connection whose writer is blocked on a peer that
// stopped draining its socket ends only through it, because a response write
// deadline cannot interrupt a socket write that the kernel buffers block.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	writeByteTimeout  = 30 * time.Second
)

// usageFlushTimeout bounds the last usage counter write after the HTTP
// drain.
const usageFlushTimeout = 10 * time.Second

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
	tel, err := telemetry.New(context.Background(), telemetry.Options{ServiceName: "ownsand-api", Level: cfg.LogLevel.SlogLevel()})
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

	var usageClient accountsv1connect.UsageServiceClient
	if cfg.Accounts.Enabled {
		tokens := accounts.NewTokenSource(accounts.TokenConfig{
			Endpoint: cfg.Accounts.TokenEndpoint, ClientID: cfg.Accounts.ClientID, ClientSecret: cfg.Accounts.ClientSecret,
		})
		usageClient = accounts.NewUsageClient(nil, cfg.Accounts.APIOrigin, tokens)
	}

	var objects objectstore.Store
	if cfg.R2.Enabled {
		objects = &objectstore.R2{Endpoint: cfg.R2.Endpoint, Bucket: cfg.R2.Bucket, AccessKeyID: cfg.R2.AccessKeyID, SecretAccessKey: cfg.R2.SecretAccessKey}
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
		NonceKey:              cfg.NonceKey,
		NonceKeyPrevious:      cfg.NonceKeyPrevious,
		CORSOrigins:           cfg.CORSOrigins,
		ClientAddressMode:     cfg.ClientAddressMode,
		STUNURLs:              cfg.STUNURLs,
		Objects:               objects,
		SnapshotPartSizeBytes: cfg.SnapshotPartSizeBytes,
		UsageClient:           usageClient,
		// The metrics are recorded whether or not the internal listener
		// runs.
		Metrics: tel.MeterProvider,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	udp, err := net.ListenPacket("udp", cfg.STUNAddr)
	if err != nil {
		_ = listener.Close()
		return err
	}
	var metricsListener net.Listener
	if cfg.MetricsAddr != "" {
		if metricsListener, err = net.Listen("tcp", cfg.MetricsAddr); err != nil {
			_ = listener.Close()
			_ = udp.Close()
			return err
		}
	}
	stunServer := stun.New(logger, cfg.STUNRateBytesPerSecond, cfg.STUNRateBurstBytes)
	srv := newServer(a.Handler(), logger)

	workersCtx, stopWorkers := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		a.Run(workersCtx)
	}()
	go func() {
		defer workers.Done()
		if err := stunServer.Serve(workersCtx, udp); err != nil {
			logger.Error("stun_server_failed", slog.Any("error", err))
		}
	}()
	// stopAndWait releases the lease (which closes the agent channels) and
	// stops the workers; it runs before the HTTP drain and again on return.
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

	logger.Info("server_started", slog.String("addr", listener.Addr().String()), slog.String("stun_addr", udp.LocalAddr().String()), slog.String("log_level", string(cfg.LogLevel)),
		slog.String("client_address_mode", string(cfg.ClientAddressMode)), slog.String("metrics_addr", metricsAddr), slog.Bool("usage_reporting", cfg.Accounts.Enabled),
		slog.Bool("otlp_metrics_export", tel.MetricsExport), slog.Bool("otlp_logs_export", tel.LogsExport))
	err = serve(ctx, stop, srv, listener, logger, shutdownTimeout, stopAndWait)
	// RPCs that ended during the drain recorded usage after the metering
	// worker stopped; write it before the process exits.
	flushCtx, cancel := context.WithTimeout(context.Background(), usageFlushTimeout)
	defer cancel()
	if ferr := a.FlushUsage(flushCtx); ferr != nil {
		logger.Error("usage_final_flush_failed", slog.Any("error", ferr))
	}
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
// TLS (h2c): Caddy terminates TLS and proxies h2c, and the bidirectional
// streams need HTTP/2 from end to end.
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
// timeout. beforeDrain runs first, so the background workers release the
// instance lease and close agent connections before the drain.
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
		// Long-lived streams that did not end within the drain are closed.
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
