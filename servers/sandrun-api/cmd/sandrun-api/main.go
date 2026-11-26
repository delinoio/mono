// SPDX-License-Identifier: FSL-1.1-ALv2

// Command sandrun-api serves the SandRun control plane. The owning contract is
// docs/servers-sandrun-api-foundation.md.
//
// The same binary is the spawn-runner hook of the Claude Code
// orchestrators that the lease holder runs: started under the name
// spawn-runner (the hooks directory links it) or as
// `sandrun-api spawn-runner`, it sends the orchestrator's spawn request to
// the parent process over the hook socket and exits with the code of the
// spawn-runner contract (package agent/claude).
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/delinoio/mono/servers/internal/telemetry"
	"github.com/delinoio/mono/servers/sandrun-api/internal/agent/claude"
	"github.com/delinoio/mono/servers/sandrun-api/internal/app"
	"github.com/delinoio/mono/servers/sandrun-api/internal/auth"
	"github.com/delinoio/mono/servers/sandrun-api/internal/config"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/github"
	"github.com/delinoio/mono/servers/sandrun-api/internal/githubapi"
	"github.com/delinoio/mono/servers/sandrun-api/internal/layers"
	"github.com/delinoio/mono/servers/sandrun-api/internal/link"
	"github.com/delinoio/mono/servers/sandrun-api/internal/ownsandclient"
)

// shutdownTimeout bounds the public HTTP drain. Server streams (dev session
// exec, WatchQueue) end with the drain. Clients resume them.
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
	if hookMode(os.Args) {
		os.Exit(claude.RunHook(os.Getenv, os.Args[0], os.Stderr))
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, stop); err != nil {
		slog.Error("server_exited", slog.Any("error", err))
		os.Exit(1)
	}
}

// hookMode reports whether the process is the spawn-runner hook.
func hookMode(args []string) bool {
	return len(args) > 0 && (filepath.Base(args[0]) == claude.HookName || len(args) > 1 && args[1] == claude.HookName)
}

func run(ctx context.Context, stop context.CancelFunc) error {
	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return err
	}
	tel, err := telemetry.New(context.Background(), telemetry.Options{ServiceName: "sandrun-api", Level: cfg.LogLevel.SlogLevel()})
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

	var gh github.Options
	if g := cfg.GitHub; g != nil {
		gh = github.Options{
			Client: githubapi.New(githubapi.Config{AppID: g.AppID, ClientID: g.ClientID, ClientSecret: g.ClientSecret, PrivateKey: g.PrivateKey, Slug: g.Slug,
				APIOrigin: g.APIOrigin, WebOrigin: g.WebOrigin}, nil, logger, nil),
			WebhookSecret: g.WebhookSecret,
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
			OwnSand:        ownsandclient.SDKFactory{BaseURL: cfg.OwnSandAPIOrigin, Logger: logger},
			OwnSandRemoval: link.RemovalOptions{ReclaimLegacy: cfg.ReclaimLegacyOwnSandRemovals},
			PublicOrigin:   cfg.PublicOrigin,
			GitHub:         gh,
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
	}

	logger.Info("server_started", slog.String("addr", listener.Addr().String()), slog.String("log_level", string(cfg.LogLevel)),
		slog.String("client_address_mode", string(cfg.ClientAddressMode)), slog.String("metrics_addr", metricsAddr),
		slog.String("secret_primary_key_id", cfg.SecretKeys.PrimaryKeyID()), slog.Bool("github_app", cfg.GitHub != nil),
		slog.Bool("otlp_metrics_export", tel.MetricsExport), slog.Bool("otlp_logs_export", tel.LogsExport))
	return runServers(ctx, stop, srv, listener, logger, shutdownTimeout, stopAndWait, metricsSrv, metricsListener)
}

// runServers shuts down public HTTP when the configured metrics listener
// fails. It waits for the metrics Serve result even if a signal starts the
// shutdown, so an overlapping metrics failure is returned to main.
func runServers(ctx context.Context, stop func(), srv *http.Server, listener net.Listener, logger *slog.Logger,
	timeout time.Duration, beforeDrain func(), metricsSrv *http.Server, metricsListener net.Listener) error {
	if metricsSrv == nil {
		return serve(ctx, stop, srv, listener, logger, timeout, beforeDrain)
	}

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	metricsDone := make(chan error, 1)
	go func() {
		err := metricsSrv.Serve(metricsListener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		if err != nil {
			logger.Error("metrics_server_failed", slog.Any("error", err))
			cancel()
		}
		metricsDone <- err
	}()

	stopSignal := func() {
		// Keep the handler installed after a metrics failure so a concurrent
		// first signal cannot interrupt the drain or the exit log.
		if ctx.Err() != nil {
			stop()
		}
	}
	err := serve(serveCtx, stopSignal, srv, listener, logger, timeout, beforeDrain)
	_ = metricsSrv.Close()
	return errors.Join(err, <-metricsDone)
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
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	var serveErr error
	select {
	case serveErr = <-serveDone:
		if errors.Is(serveErr, http.ErrServerClosed) {
			return nil
		}
	case <-ctx.Done():
		stop()
	}
	logger.Info("server_shutdown_started")
	beforeDrain()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	drainErr := srv.Shutdown(shutdownCtx)
	if errors.Is(drainErr, context.DeadlineExceeded) {
		// Streams that did not end within the drain are closed.
		drainErr = srv.Close()
	}
	if serveErr == nil {
		serveErr = <-serveDone
	}
	if errors.Is(serveErr, http.ErrServerClosed) {
		serveErr = nil
	}
	if err := errors.Join(serveErr, drainErr); err != nil {
		return err
	}
	logger.Info("server_shutdown_completed")
	return nil
}
