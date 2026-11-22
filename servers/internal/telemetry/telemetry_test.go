// SPDX-License-Identifier: FSL-1.1-ALv2

package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

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
	mu    sync.Mutex
	paths []string
	logs  []string
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
