// SPDX-License-Identifier: FSL-1.1-ALv2

// Command ownsand-api serves the OwnSand control plane.
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

// usageFlushTimeout bounds the whole usage finish after the HTTP drain: the
// wait for the requests that can still record usage (at most
// usageDrainTimeout of it) and the last usage counter write, which gets the
// rest.
const usageFlushTimeout = 10 * time.Second

// usageDrainTimeout bounds the wait for requests that can still record usage
// after the HTTP drain. A handler that never exits cannot hold the shutdown
// longer than this; the service then reports an incomplete drain.
const usageDrainTimeout = 5 * time.Second

// errUsageDrainIncomplete is returned by run when requests that can record
// usage were still running when the usage drain ended, so the final flush
// may miss their usage.
var errUsageDrainIncomplete = errors.New("usage drain incomplete")

// usageFinisher is the part of the application that finishes usage metering
// at shutdown.
type usageFinisher interface {
	DrainUsage(ctx context.Context) error
	FlushUsageFinal(ctx context.Context) error
}

// finishUsage runs after the HTTP server stopped. RPCs that ended during the
// HTTP drain recorded usage after the metering worker stopped, so it waits
// (bounded by drainTimeout) for the requests that can still record usage,
// then writes the counts and closes the recorder within the flush timeout.
// It returns errUsageDrainIncomplete when the drain ended before those
// requests finished: their usage may be lost and a count that arrives after
// the flush is logged by the recorder. It also returns the final flush error
// if the write fails.
func finishUsage(a usageFinisher, logger *slog.Logger, flushTimeout, drainTimeout time.Duration) error {
	flushCtx, cancel := context.WithTimeout(context.Background(), flushTimeout)
	defer cancel()
	var incomplete error
	drainCtx, cancelDrain := context.WithTimeout(flushCtx, drainTimeout)
	if derr := a.DrainUsage(drainCtx); derr != nil {
		logger.Error("usage_drain_incomplete", slog.Any("error", derr))
		incomplete = fmt.Errorf("%w: %w", errUsageDrainIncomplete, derr)
	}
	cancelDrain()
	if ferr := a.FlushUsageFinal(flushCtx); ferr != nil {
		logger.Error("usage_final_flush_failed", slog.Any("error", ferr))
		return errors.Join(incomplete, ferr)
	}
	return incomplete
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		logServerExited(slog.Default(), err)
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

	// Only the HTTP and STUN bind errors carry a startup failure reason: they
	// are the addresses another process can take between the choice of a
	// free port and the bind, which a supervisor retries on new ports.
	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return classifyBindError(err)
	}
	udp, err := net.ListenPacket("udp", cfg.STUNAddr)
	if err != nil {
		_ = listener.Close()
		return classifyBindError(err)
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
	usageErr := finishUsage(a, logger, usageFlushTimeout, usageDrainTimeout)
	if metricsSrv != nil {
		_ = metricsSrv.Close()
	}
	return errors.Join(err, usageErr)
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
