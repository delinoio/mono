// SPDX-License-Identifier: FSL-1.1-ALv2

// Command accounts-api serves the Delino account and billing API.
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

	"github.com/delinoio/mono/servers/accounts-api/internal/app"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/billing"
	"github.com/delinoio/mono/servers/accounts-api/internal/config"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/server"
	"github.com/delinoio/mono/servers/internal/telemetry"
)

const shutdownTimeout = 10 * time.Second

// HTTP server timeouts. They are fixed constants rather than configuration.
//
// readHeaderTimeout bounds reading the request headers. net/http clears that
// deadline once the headers are read, so without readTimeout a client could
// declare a body and then stall forever. readTimeout bounds the whole request
// read (headers and body) and must not be below readHeaderTimeout. writeTimeout
// bounds writing the response. idleTimeout bounds a keep-alive connection that
// is waiting for its next request; without it the connection stays open until
// the client closes it.
//
// writeTimeout also caps the lifetime of any single response, so it must be
// revisited (or lifted per handler through http.ResponseController) before a
// long-lived streaming RPC is added.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	writeTimeout      = 30 * time.Second
	idleTimeout       = 120 * time.Second
)

func main() {
	// Keep startup failures in the same JSON format as the server logs.
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

	tel, err := telemetry.New(context.Background(), telemetry.Options{ServiceName: "accounts-api", Level: cfg.LogLevel.SlogLevel()})
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

	ctx, stop := shutdownContext()
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
	} else {
		logger.Info("db_migrations_skipped", slog.String("reason", config.EnvRunMigrations+" is false"))
	}

	// The metrics exist also without METRICS_ADDR, so the code that records
	// them never checks whether the listener runs.
	appMetrics, err := metrics.New(tel.MeterProvider)
	if err != nil {
		return err
	}
	handler, billingService, err := newHandler(cfg, database, logger, appMetrics)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}

	// Bind the internal metrics listener before any background task starts. A
	// bind failure then returns before the billing worker exists, so run never
	// returns while a worker, and the Paddle call it may start, still runs.
	var metricsListener net.Listener
	if cfg.MetricsAddr != "" {
		metricsListener, err = net.Listen("tcp", cfg.MetricsAddr)
		if err != nil {
			_ = listener.Close()
			return err
		}
	}

	srv := newServer(handler, logger)

	logger.Info("server_started", slog.String("addr", listener.Addr().String()), slog.String("log_level", string(cfg.LogLevel)),
		slog.Bool("otlp_metrics_export", tel.MetricsExport), slog.Bool("otlp_logs_export", tel.LogsExport))
	worker := func(background context.Context) {
		billingService.RunWorker(background, billing.DefaultWorkerInterval)
	}
	var serveMetrics func(context.Context) error
	if metricsListener != nil {
		metricsHandler := tel.MetricsHandler(slog.NewLogLogger(logger.Handler(), slog.LevelError))
		serveMetrics = func(background context.Context) error {
			return metrics.Serve(background, metricsListener, metricsHandler, logger)
		}
	}
	return runServers(ctx, stop, srv, listener, logger, shutdownTimeout, worker, serveMetrics)
}

// runServers serves the public srv on listener together with the background
// tasks: the billing worker and, when serveMetrics is not nil, the internal
// metrics listener. It returns after srv has drained and both tasks have
// returned, so no task outlives it. The worker finishes the run in progress, so
// a Paddle call that has started records its result.
//
// The metrics listener is not optional infrastructure once it is bound: when
// serveMetrics fails by itself (for example a permanent Accept error), runServers
// logs the error and starts the bounded shutdown of srv. If no signal has
// arrived at drain entry, the signal registration stays active so the first
// signal cannot interrupt the drain. The original metrics error is joined into
// the result even if a signal races with the failure.
//
// worker and serveMetrics receive a context that is cancelled only after srv
// has drained. runServers takes them as functions so tests can inject failures
// without OS signals.
func runServers(ctx context.Context, stop func(), srv *http.Server, listener net.Listener, logger *slog.Logger, timeout time.Duration,
	worker func(context.Context), serveMetrics func(context.Context) error) error {
	// Only the original context tells us that a signal arrived.
	serveCtx, cancelServe := context.WithCancel(ctx)
	defer cancelServe()

	background, stopBackground := context.WithCancel(context.Background())
	defer stopBackground()
	backgroundErrs := []<-chan error{runAsync(func() error {
		worker(background)
		return nil
	})}
	if serveMetrics != nil {
		backgroundErrs = append(backgroundErrs, runAsync(func() error {
			err := serveMetrics(background)
			// metrics.Serve returns nil after a shutdown, so an error here, while
			// background is still live, is a terminal listener failure. The
			// error is also returned through the channel and joined below.
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("metrics_listener_failed", slog.Any("error", err))
				cancelServe()
			}
			return err
		}))
	}

	stopSignal := func() {
		if ctx.Err() != nil {
			stop()
		}
	}
	serveErr := serve(serveCtx, stopSignal, srv, listener, logger, timeout)
	stopBackground()
	errs := []error{serveErr}
	for _, errc := range backgroundErrs {
		if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// runAsync runs fn in a goroutine and returns the channel of its result.
func runAsync(fn func() error) <-chan error {
	errc := make(chan error, 1)
	go func() { errc <- fn() }()
	return errc
}

// newHandler builds the production handler and the billing service, whose
// worker run starts, from the configuration.
func newHandler(cfg config.Config, database *db.DB, logger *slog.Logger, appMetrics *metrics.Metrics) (http.Handler, *billing.Service, error) {
	sender, err := email.NewResend(email.ResendConfig{APIKey: cfg.ResendAPIKey, From: cfg.EmailFrom, Logger: logger})
	if err != nil {
		return nil, nil, err
	}
	dir := directory.NewLogto(directory.LogtoConfig{
		Endpoint:     cfg.LogtoManagementEndpoint,
		ClientID:     cfg.LogtoM2MClientID,
		ClientSecret: cfg.LogtoM2MClientSecret,
		Logger:       logger,
	})
	billingService := billing.New(billing.Config{
		DB:                  database,
		Paddle:              paddle.New(paddle.Config{BaseURL: cfg.PaddleEnvironment.BaseURL(), APIKey: cfg.PaddleAPIKey}),
		Directory:           dir,
		Email:               sender,
		Metrics:             appMetrics,
		Logger:              logger,
		WebhookSecret:       cfg.PaddleWebhookSecret,
		SubscriptionPriceID: cfg.PaddleSubscriptionPriceID,
		TopUpProductID:      cfg.PaddleTopUpProductID,
		WebOrigin:           cfg.WebOrigin,
	})
	return app.Handler(app.Options{
		DB:      database,
		Billing: billingService,
		Validator: auth.NewLogtoValidator(auth.LogtoConfig{
			Issuer:   cfg.LogtoIssuer,
			Audience: cfg.LogtoAudience,
			Logger:   logger,
		}),
		Directory:   dir,
		Email:       sender,
		Logger:      logger,
		Audience:    cfg.LogtoAudience,
		CORSOrigins: cfg.CORSOrigins,
		WebOrigin:   cfg.WebOrigin,

		UsageClients: cfg.UsageServiceClients,
	}), billingService, nil
}

// shutdownContext returns a context that is cancelled by the first SIGINT or
// SIGTERM, and the stop function that releases the signal registration.
//
// The registration stays installed until stop runs. At drain entry, runServers
// releases the registration if it has observed a signal. A second signal then
// reaches the default disposition and terminates the process.
func shutdownContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// newServer is the single production construction point of the http.Server.
//
// net/http reports its own internal errors (a recovered handler panic with its
// stack trace, a superfluous WriteHeader call and TLS handshake errors)
// through Server.ErrorLog. When ErrorLog is nil it falls back
// to the standard library's default logger. After slog.SetDefault that logger
// forwards to the slog default handler at INFO level, so these failures would
// be recorded as INFO and dropped when LOG_LEVEL is warn or error; without
// slog.SetDefault they would be plain text on stderr. Setting ErrorLog routes
// them through logger's handler as ERROR records independent of the global
// default, so every server log stays JSON on standard output.
//
// DisableGeneralOptionsHandler stops net/http from answering a server-wide
// "OPTIONS *" request itself, which would bypass Handler and the request log.
// server.NewHandler answers and logs that request instead; a Handler passed to
// newServer must do the same if it needs to serve "OPTIONS *".
//
// net/http rejects some requests before it calls Handler, so the request logger
// inside Handler never sees them: an unsupported Expect header (417), a
// malformed request (400), an oversized head (431), an unsupported
// Transfer-Encoding (501) or protocol version (505) and a ReadHeaderTimeout
// expiry. serve wraps the listener with server.ObserveListener, which logs
// these rejections, and ConnContext and ConnState here pair each connection
// with its observer and tell it about hijacked connections and about finished
// responses. MaxHeaderBytes is
// set explicitly because the observer derives its own head limit from the same
// constant.
func newServer(handler http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{
		Handler:                      handler,
		ConnContext:                  server.ConnContext,
		ConnState:                    server.ConnState,
		MaxHeaderBytes:               server.MaxHeaderBytes,
		DisableGeneralOptionsHandler: true,
		ReadHeaderTimeout:            readHeaderTimeout,
		ReadTimeout:                  readTimeout,
		WriteTimeout:                 writeTimeout,
		IdleTimeout:                  idleTimeout,
		ErrorLog:                     slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
}

// serve runs srv on listener until Serve fails or ctx is done, then shuts the
// server down within timeout. It takes the shutdown trigger as a context so
// tests can drive it without OS signals.
//
// stop is the callback that can release the signal registration. When ctx is
// done first, serve calls it before logging server_shutdown_started and starting
// the drain. When Serve fails first, serve drains with the signal registration
// still active and the caller's deferred stop handles cleanup.
// stop must be safe to call more than once.
//
// Request contexts derive from a server-lifetime context that is deliberately
// not ctx: cancelling ctx only starts the drain, so in-flight handlers that
// honour r.Context() can finish within timeout. The server-lifetime context is
// cancelled once the drain has ended, which cuts off handlers that outlived
// the timeout. serve sets srv.BaseContext and must not be called twice for the
// same srv.
func serve(ctx context.Context, stop func(), srv *http.Server, listener net.Listener, logger *slog.Logger, timeout time.Duration) error {
	baseCtx, cancelBase := context.WithCancel(context.Background())
	defer cancelBase()
	srv.BaseContext = func(net.Listener) context.Context { return baseCtx }

	// Log requests that net/http rejects before the handler runs; see
	// newServer.
	observed := server.ObserveListener(listener, logger)
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.Serve(observed)
	}()
	return awaitServe(ctx, stop, srv, serveErr, logger, timeout)
}

// awaitServe waits for the Serve result on serveErr or for ctx to be done. It
// takes the result channel so tests can inject a Serve outcome deterministically.
// Either trigger starts the bounded drain unless Serve has closed normally.
func awaitServe(ctx context.Context, stop func(), srv *http.Server, serveErr <-chan error, logger *slog.Logger, timeout time.Duration) error {
	var listenerErr error
	select {
	case listenerErr = <-serveErr:
		if listenerErr == nil || errors.Is(listenerErr, http.ErrServerClosed) {
			return nil
		}
		serveErr = nil
	case <-ctx.Done():
		// Release the signal registration synchronously when a signal led the
		// drain so that a second signal can terminate the process promptly.
		stop()
	}

	logger.Info("server_shutdown_started")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shutdownErr := srv.Shutdown(shutdownCtx)
	if shutdownErr != nil {
		logger.Error("server_shutdown_failed", slog.Any("error", shutdownErr))
	}

	// If cancellation won the select, collect the Serve result now to retain
	// any concurrent listener failure. Shutdown makes Serve return.
	if serveErr != nil {
		if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
			listenerErr = err
		}
	}
	if err := errors.Join(shutdownErr, listenerErr); err != nil {
		return err
	}
	logger.Info("server_shutdown_completed")
	return nil
}
