// SPDX-License-Identifier: FSL-1.1-ALv2

package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	collogspb "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	"google.golang.org/protobuf/proto"
)

func noEnv(string) (string, bool) { return "", false }

func scrape(t *testing.T, tel *Telemetry) string {
	t.Helper()
	srv := httptest.NewServer(tel.MetricsHandler(nil))
	defer srv.Close()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics = %d", resp.StatusCode)
	}
	return string(body)
}

// TestPrometheusNames checks that the exposition keeps Prometheus names: a
// _total suffix on counters, a unit suffix, no scope labels, and the Go
// runtime metrics.
func TestPrometheusNames(t *testing.T) {
	ctx := context.Background()
	var out bytes.Buffer
	tel, err := New(ctx, Options{ServiceName: "test-api", Stdout: &out, LookupEnv: noEnv})
	if err != nil {
		t.Fatal(err)
	}
	defer tel.Shutdown(ctx)
	if tel.MetricsExport || tel.LogsExport {
		t.Fatalf("export on without an endpoint: metrics %v, logs %v", tel.MetricsExport, tel.LogsExport)
	}
	meter := tel.MeterProvider.Meter("test")
	counter, _ := meter.Int64Counter("test_events", metric.WithDescription("Events."))
	counter.Add(ctx, 2, metric.WithAttributes(attribute.String("kind", "a")))
	gauge, _ := meter.Float64Gauge("test_lag", metric.WithUnit("s"))
	gauge.Record(ctx, 1.5)

	body := scrape(t, tel)
	for _, want := range []string{`test_events_total{kind="a"} 2`, `test_lag_seconds 1.5`, `go_goroutine_count`, `# HELP test_events_total Events.`} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics lack %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "otel_scope_name") {
		t.Errorf("metrics have scope labels:\n%s", body)
	}

	health := httptest.NewServer(tel.MetricsHandler(nil))
	defer health.Close()
	resp, err := http.Get(health.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/healthz = %d, want 404", resp.StatusCode)
	}

	tel.Logger.Debug("hidden")
	tel.Logger.Info("shown", slog.String("k", "v"))
	if strings.Contains(out.String(), "hidden") || !strings.Contains(out.String(), `"msg":"shown","k":"v"`) {
		t.Errorf("stdout = %s", out.String())
	}
}

// otlpServer records the OTLP/HTTP requests it receives.
type otlpServer struct {
	mu      sync.Mutex
	paths   []string
	headers []http.Header
	logs    []string
}

func (s *otlpServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		zr, err := gzip.NewReader(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		body = zr
	}
	data, _ := io.ReadAll(body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, r.URL.Path)
	s.headers = append(s.headers, r.Header.Clone())
	if r.URL.Path == "/v1/logs" {
		var req collogspb.ExportLogsServiceRequest
		if err := proto.Unmarshal(data, &req); err == nil {
			for _, rl := range req.ResourceLogs {
				for _, sl := range rl.ScopeLogs {
					for _, lr := range sl.LogRecords {
						s.logs = append(s.logs, lr.Body.GetStringValue())
					}
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
}

// TestOTLPExport checks that OTEL_EXPORTER_OTLP_ENDPOINT turns on the export
// of metrics and of the log records at or above the logger level.
func TestOTLPExport(t *testing.T) {
	clearOTLPEnv(t)
	ctx := context.Background()
	collector := &otlpServer{}
	srv := httptest.NewServer(collector)
	defer srv.Close()
	t.Setenv(EnvOTLPEndpoint, srv.URL)

	var out bytes.Buffer
	tel, err := New(ctx, Options{ServiceName: "test-api", Stdout: &out, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	if !tel.MetricsExport || !tel.LogsExport {
		t.Fatalf("export off with an endpoint: metrics %v, logs %v", tel.MetricsExport, tel.LogsExport)
	}
	counter, _ := tel.MeterProvider.Meter("test").Int64Counter("test_events")
	counter.Add(ctx, 1)
	tel.Logger.Debug("hidden")
	tel.Logger.Info("exported")
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	collector.mu.Lock()
	defer collector.mu.Unlock()
	if !slices.Contains(collector.paths, "/v1/metrics") || !slices.Contains(collector.paths, "/v1/logs") {
		t.Errorf("OTLP paths = %v", collector.paths)
	}
	if !slices.Contains(collector.logs, "exported") || slices.Contains(collector.logs, "hidden") {
		t.Errorf("exported log bodies = %v", collector.logs)
	}
	if !strings.Contains(out.String(), "exported") {
		t.Errorf("stdout = %s", out.String())
	}
}

// clearOTLPEnv removes every OTEL_EXPORTER_OTLP_* variable of the parent
// process for the duration of the test, so that inherited signal-specific
// endpoints, headers, compression and certificates cannot redirect or alter
// the export to the fixture collector. t.Setenv records the original value and
// restores it when the test ends.
func clearOTLPEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "OTEL_EXPORTER_OTLP_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
}

// stalledCollector accepts OTLP requests and never answers them: each request
// blocks until the client gives up or the test ends.
func stalledCollector(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	return srv
}

// TestShutdownFlushesLogsWhileMetricsExportStalls checks that a metrics
// collector that never answers does not use up the shutdown deadline before
// the log export starts: both exports run in parallel under the one deadline.
func TestShutdownFlushesLogsWhileMetricsExportStalls(t *testing.T) {
	clearOTLPEnv(t)
	ctx := context.Background()
	stalled := stalledCollector(t)
	logCollector := &otlpServer{}
	logSrv := httptest.NewServer(logCollector)
	defer logSrv.Close()
	t.Setenv(EnvOTLPMetricsEndpoint, stalled.URL)
	t.Setenv(EnvOTLPLogsEndpoint, logSrv.URL+"/v1/logs")

	var out bytes.Buffer
	tel, err := New(ctx, Options{ServiceName: "test-api", Stdout: &out, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	counter, _ := tel.MeterProvider.Meter("test").Int64Counter("test_events")
	counter.Add(ctx, 1)
	tel.Logger.Info("queued")

	const deadline = 300 * time.Millisecond
	shutdownCtx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()
	start := time.Now()
	err = tel.Shutdown(shutdownCtx)
	elapsed := time.Since(start)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want a deadline error from the stalled metrics export", err)
	}
	if elapsed > deadline+2*time.Second {
		t.Errorf("Shutdown took %v with a %v deadline", elapsed, deadline)
	}
	logCollector.mu.Lock()
	defer logCollector.mu.Unlock()
	if !slices.Contains(logCollector.logs, "queued") {
		t.Errorf("the log collector received %v, want the queued record", logCollector.logs)
	}
}

// TestShutdownReportsBothExportErrors checks that Shutdown joins the errors of
// the metrics and the log export when both collectors stall.
func TestShutdownReportsBothExportErrors(t *testing.T) {
	clearOTLPEnv(t)
	ctx := context.Background()
	t.Setenv(EnvOTLPMetricsEndpoint, stalledCollector(t).URL)
	t.Setenv(EnvOTLPLogsEndpoint, stalledCollector(t).URL)

	tel, err := New(ctx, Options{ServiceName: "test-api", Stdout: io.Discard, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	counter, _ := tel.MeterProvider.Meter("test").Int64Counter("test_events")
	counter.Add(ctx, 1)
	tel.Logger.Info("queued")

	shutdownCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	err = tel.Shutdown(shutdownCtx)
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok || len(joined.Unwrap()) != 2 {
		t.Fatalf("Shutdown error = %v, want the errors of both providers joined", err)
	}
	for _, e := range joined.Unwrap() {
		if !errors.Is(e, context.DeadlineExceeded) {
			t.Errorf("joined error %v is not a deadline error", e)
		}
	}
}

// TestShutdownWithoutLogExport checks that Shutdown works when only metrics
// are exported, so that there is no logger provider.
func TestShutdownWithoutLogExport(t *testing.T) {
	clearOTLPEnv(t)
	ctx := context.Background()
	collector := &otlpServer{}
	srv := httptest.NewServer(collector)
	defer srv.Close()
	t.Setenv(EnvOTLPMetricsEndpoint, srv.URL+"/v1/metrics") // signal-specific endpoints are used as given

	tel, err := New(ctx, Options{ServiceName: "test-api", Stdout: io.Discard, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	if !tel.MetricsExport || tel.LogsExport {
		t.Fatalf("export: metrics %v, logs %v", tel.MetricsExport, tel.LogsExport)
	}
	counter, _ := tel.MeterProvider.Meter("test").Int64Counter("test_events")
	counter.Add(ctx, 1)
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if !slices.Contains(collector.paths, "/v1/metrics") || slices.Contains(collector.paths, "/v1/logs") {
		t.Errorf("OTLP paths = %v", collector.paths)
	}
}

// TestOTLPExportIgnoresInheritedSettings checks that OTLP settings that the
// test process inherits (signal-specific endpoints, generic and
// signal-specific headers and compression) cannot redirect or alter the export
// of the fixture tests. The requests reach only the fixture collector, with
// no inherited headers or compression.
func TestOTLPExportIgnoresInheritedSettings(t *testing.T) {
	alternate := &otlpServer{}
	altSrv := httptest.NewServer(alternate)
	defer altSrv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", altSrv.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", altSrv.URL+"/v1/logs")
	t.Setenv("OTEL_EXPORTER_OTLP_HEADERS", "X-Inherited=generic")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_HEADERS", "X-Inherited=metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_LOGS_HEADERS", "X-Inherited=logs")
	t.Setenv("OTEL_EXPORTER_OTLP_COMPRESSION", "gzip")
	clearOTLPEnv(t)

	ctx := context.Background()
	collector := &otlpServer{}
	srv := httptest.NewServer(collector)
	defer srv.Close()
	t.Setenv(EnvOTLPEndpoint, srv.URL)

	tel, err := New(ctx, Options{ServiceName: "test-api", Stdout: io.Discard, Level: slog.LevelInfo})
	if err != nil {
		t.Fatal(err)
	}
	counter, _ := tel.MeterProvider.Meter("test").Int64Counter("test_events")
	counter.Add(ctx, 1)
	tel.Logger.Info("exported")
	if err := tel.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	alternate.mu.Lock()
	if len(alternate.paths) != 0 {
		t.Errorf("the alternate collector received %v, want no request", alternate.paths)
	}
	alternate.mu.Unlock()
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if !slices.Contains(collector.paths, "/v1/metrics") || !slices.Contains(collector.paths, "/v1/logs") {
		t.Errorf("OTLP paths = %v", collector.paths)
	}
	for i, h := range collector.headers {
		if v := h.Get("X-Inherited"); v != "" {
			t.Errorf("request %d to %s carries the inherited header X-Inherited=%s", i, collector.paths[i], v)
		}
		if encoding := h.Get("Content-Encoding"); encoding != "" {
			t.Errorf("request %d to %s has Content-Encoding %q, want empty", i, collector.paths[i], encoding)
		}
	}
}
