// SPDX-License-Identifier: FSL-1.1-ALv2

// Package telemetry sets up the OpenTelemetry SDK for a server: a meter
// provider, the Go runtime metrics, a Prometheus exposition handler for the
// internal metrics listener and a structured logger. When the standard
// OpenTelemetry environment variables configure an OTLP endpoint, it also
// exports metrics and log records over OTLP/HTTP (for example to Grafana
// Cloud). Without an OTLP endpoint, nothing leaves the process.
//
// The rule that metrics, logs and traces use the OpenTelemetry SDK is in
// docs/repository-defaults.md (Repository-Wide Rules).
package telemetry

import (
	"context"
	"errors"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	otelprometheus "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
)

// Environment variables that turn on OTLP export. The OTLP exporters read
// these and the other standard OTEL_EXPORTER_OTLP_* variables (headers,
// compression, timeout) themselves; see
// https://opentelemetry.io/docs/specs/otel/protocol/exporter/.
const (
	EnvOTLPEndpoint        = "OTEL_EXPORTER_OTLP_ENDPOINT"
	EnvOTLPMetricsEndpoint = "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"
	EnvOTLPLogsEndpoint    = "OTEL_EXPORTER_OTLP_LOGS_ENDPOINT"
)

// Options configure New.
type Options struct {
	// ServiceName is the default service.name resource attribute.
	// OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES override it.
	ServiceName string
	// Level is the minimum level of the logger, for both standard output and
	// OTLP export.
	Level slog.Leveler
	// Stdout receives the JSON log records. Nil selects os.Stdout.
	Stdout io.Writer
	// LookupEnv reads the variables that turn on OTLP export. Nil selects
	// os.LookupEnv.
	LookupEnv func(string) (string, bool)
}

// Telemetry is the OpenTelemetry SDK state of one server process.
type Telemetry struct {
	// MeterProvider creates the meters of the server metrics.
	MeterProvider metric.MeterProvider
	// Logger writes JSON records to standard output and, when OTLP log
	// export is on, also sends them to the OpenTelemetry SDK.
	Logger *slog.Logger
	// MetricsExport and LogsExport tell whether OTLP export is on.
	MetricsExport bool
	LogsExport    bool

	registry       *prometheus.Registry
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *sdklog.LoggerProvider
}

// New sets up the OpenTelemetry SDK. Call Shutdown before the process exits
// to flush OTLP export.
func New(ctx context.Context, opts Options) (*Telemetry, error) {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	level := opts.Level
	if level == nil {
		level = slog.LevelInfo
	}

	// The service name comes first so that OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES, which WithFromEnv reads, override it.
	res, err := resource.New(ctx,
		resource.WithAttributes(semconv.ServiceName(opts.ServiceName)),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, err
	}

	t := &Telemetry{
		registry:      prometheus.NewRegistry(),
		MetricsExport: envSet(lookup, EnvOTLPEndpoint) || envSet(lookup, EnvOTLPMetricsEndpoint),
		LogsExport:    envSet(lookup, EnvOTLPEndpoint) || envSet(lookup, EnvOTLPLogsEndpoint),
	}

	// The Prometheus exporter keeps the metric names that the internal
	// listener served before the move to the OpenTelemetry SDK: it adds the
	// _total suffix to counters and the unit suffix (for example _seconds),
	// and the scope labels are left out so that every series keeps its label
	// set.
	promExporter, err := otelprometheus.New(otelprometheus.WithRegisterer(t.registry), otelprometheus.WithoutScopeInfo())
	if err != nil {
		return nil, err
	}
	meterOpts := []sdkmetric.Option{sdkmetric.WithResource(res), sdkmetric.WithReader(promExporter)}
	if t.MetricsExport {
		exp, err := otlpmetrichttp.New(ctx)
		if err != nil {
			return nil, err
		}
		meterOpts = append(meterOpts, sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
	}
	t.meterProvider = sdkmetric.NewMeterProvider(meterOpts...)
	t.MeterProvider = t.meterProvider
	if err := runtime.Start(runtime.WithMeterProvider(t.meterProvider)); err != nil {
		_ = t.meterProvider.Shutdown(ctx)
		return nil, err
	}

	stdoutHandler := slog.NewJSONHandler(stdout, &slog.HandlerOptions{Level: level})
	// The SDK reports export failures through the global error handler, which
	// writes plain text to standard error by default. Write them as JSON to
	// standard output instead, and only there: sending an export failure back
	// to the OTLP log exporter would repeat it.
	errorLogger := slog.New(stdoutHandler)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		errorLogger.Error("telemetry_export_failed", slog.Any("error", err))
	}))
	handler := slog.Handler(stdoutHandler)
	if t.LogsExport {
		exp, err := otlploghttp.New(ctx)
		if err != nil {
			_ = t.meterProvider.Shutdown(ctx)
			return nil, err
		}
		t.loggerProvider = sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(exp)))
		bridge := otelslog.NewHandler(opts.ServiceName, otelslog.WithLoggerProvider(t.loggerProvider))
		handler = slog.NewMultiHandler(handler, &levelHandler{Handler: bridge, level: level})
	}
	t.Logger = slog.New(handler)
	return t, nil
}

// MetricsHandler serves GET /metrics in the Prometheus text format. Mount it
// only on the internal metrics listener. errorLog receives exposition errors.
func (t *Telemetry) MetricsHandler(errorLog *log.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", promhttp.HandlerFor(t.registry, promhttp.HandlerOpts{ErrorLog: errorLog}))
	return mux
}

// Shutdown flushes and stops OTLP export. It returns when ctx is done at the
// latest. The metrics and the log export shut down in parallel under the same
// ctx: run one after the other, a metrics collector that does not answer would
// use up the whole deadline and the queued log records would never be sent.
// The returned error joins the errors of both providers.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var metricsErr, logsErr error
	var wg sync.WaitGroup
	wg.Go(func() { metricsErr = t.meterProvider.Shutdown(ctx) })
	if t.loggerProvider != nil {
		wg.Go(func() { logsErr = t.loggerProvider.Shutdown(ctx) })
	}
	wg.Wait()
	return errors.Join(metricsErr, logsErr)
}

// ShutdownTimeout is a suitable bound for Shutdown at process exit.
const ShutdownTimeout = 5 * time.Second

func envSet(lookup func(string) (string, bool), name string) bool {
	v, _ := lookup(name)
	return strings.TrimSpace(v) != ""
}

// levelHandler drops records below level. The OpenTelemetry log bridge is
// enabled for every level, so without it the bridge would export the debug
// records that LOG_LEVEL turns off for standard output.
type levelHandler struct {
	slog.Handler
	level slog.Leveler
}

func (h *levelHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.level.Level() && h.Handler.Enabled(ctx, l)
}

func (h *levelHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithAttrs(attrs), level: h.level}
}

func (h *levelHandler) WithGroup(name string) slog.Handler {
	return &levelHandler{Handler: h.Handler.WithGroup(name), level: h.level}
}
