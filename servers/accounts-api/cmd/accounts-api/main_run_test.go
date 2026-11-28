// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"log"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/config"
	"github.com/delinoio/mono/servers/internal/telemetry"
)

const (
	otelResourceAttributes = "OTEL_RESOURCE_ATTRIBUTES"
	otelServiceName        = "OTEL_SERVICE_NAME"
)

// setRunEnv sets the configuration that lets run start without external
// services. The database pool opens lazily and RUN_MIGRATIONS=false skips the
// migrations. The fake dispatcher keeps billing work from contacting the
// database. Empty OpenTelemetry settings keep resource detection and export
// independent of the test process environment.
func setRunEnv(t *testing.T, metricsAddr string) {
	t.Helper()
	env := map[string]string{
		"DATABASE_URL":                 "postgres://accounts@127.0.0.1:1/accounts?connect_timeout=1",
		"RUN_MIGRATIONS":               "false",
		"ADDR":                         "127.0.0.1:0",
		"METRICS_ADDR":                 metricsAddr,
		config.EnvLogLevel:             "error",
		"LOGTO_ISSUER":                 "http://127.0.0.1:1/oidc",
		"LOGTO_MANAGEMENT_ENDPOINT":    "http://127.0.0.1:1",
		"LOGTO_M2M_CLIENT_ID":          "run-test",
		"LOGTO_M2M_CLIENT_SECRET":      "run-test-secret",
		"RESEND_API_KEY":               "re_run_test",
		"EMAIL_FROM":                   "billing@example.com",
		"PADDLE_ENVIRONMENT":           "sandbox",
		"PADDLE_API_KEY":               "pdl_run_test",
		"PADDLE_WEBHOOK_SECRET":        "pdl_ntfset_run_test",
		"PADDLE_SUBSCRIPTION_PRICE_ID": "pri_01aaaaaaaaaaaaaaaaaaaaaaaa",
		"PADDLE_TOP_UP_PRODUCT_ID":     "pro_01bbbbbbbbbbbbbbbbbbbbbbbb",

		otelResourceAttributes:           "",
		otelServiceName:                  "",
		telemetry.EnvOTLPEndpoint:        "",
		telemetry.EnvOTLPMetricsEndpoint: "",
		telemetry.EnvOTLPLogsEndpoint:    "",
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

// TestRunFailsOnMetricsBindErrorBeforeStartingWorker verifies that a metrics
// listener that cannot be bound fails run before the billing worker starts, so
// no Paddle call can begin after run has returned.
func TestRunFailsOnMetricsBindErrorBeforeStartingWorker(t *testing.T) {
	inheritedResources := map[string]string{
		otelResourceAttributes: "invalid_fixture_attribute",
		otelServiceName:        "inherited-accounts-test",
	}
	for name, value := range inheritedResources {
		t.Setenv(name, value)
	}

	held := newTestListener(t)
	tests := []struct {
		name        string
		metricsAddr string
	}{
		{name: "address in use", metricsAddr: held.Addr().String()},
		{name: "host that does not resolve", metricsAddr: "nonexistent.invalid:9100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRunEnv(t, tt.metricsAddr)
			assertRunMetricsBindError(t)
			if got := os.Getenv(otelServiceName); got != "" {
				t.Errorf("%s = %q inside the fixture; want empty", otelServiceName, got)
			}
		})
		for name, want := range inheritedResources {
			if got := os.Getenv(name); got != want {
				t.Errorf("after %s, %s = %q; want %q", tt.name, name, got, want)
			}
		}
	}
}

func TestRunIgnoresInheritedOTLPEndpoints(t *testing.T) {
	t.Setenv(otelResourceAttributes, "fixture.source=inherited")
	t.Setenv(otelServiceName, "inherited-accounts-test")
	held := newTestListener(t)
	endpoints := []string{
		telemetry.EnvOTLPEndpoint,
		telemetry.EnvOTLPMetricsEndpoint,
		telemetry.EnvOTLPLogsEndpoint,
	}
	for _, endpoint := range endpoints {
		t.Run(endpoint, func(t *testing.T) {
			var requests atomic.Int64
			collector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer collector.Close()
			for _, name := range endpoints {
				t.Setenv(name, "")
			}
			t.Setenv(endpoint, collector.URL)

			t.Run("bind", func(t *testing.T) {
				setRunEnv(t, held.Addr().String())
				// Emit the migration-skip log so an enabled log exporter has
				// a record to flush during shutdown.
				t.Setenv(config.EnvLogLevel, "info")
				assertRunMetricsBindError(t)
			})
			if got := requests.Load(); got != 0 {
				t.Errorf("collector received %d requests; want 0", got)
			}
			if got := os.Getenv(endpoint); got != collector.URL {
				t.Errorf("%s = %q after the fixture; want %q", endpoint, got, collector.URL)
			}
		})
	}
}

func assertRunMetricsBindError(t *testing.T) {
	t.Helper()
	originalLogger := slog.Default()
	originalLogWriter, originalLogFlags := log.Writer(), log.Flags()
	t.Cleanup(func() {
		slog.SetDefault(originalLogger)
		log.SetOutput(originalLogWriter)
		log.SetFlags(originalLogFlags)
	})

	dispatches := 0
	dispatchWorker := func(func() error) <-chan error {
		dispatches++
		result := make(chan error, 1)
		result <- nil
		return result
	}

	err := run(dispatchWorker)
	if err == nil {
		t.Fatal("run succeeded; want the metrics listen error")
	}
	var opErr *net.OpError
	if !errors.As(err, &opErr) || opErr.Op != "listen" {
		t.Fatalf("run error = %v; want a listen error", err)
	}
	if dispatches != 0 {
		t.Errorf("billing worker dispatches = %d; want 0 after a metrics listen error", dispatches)
	}
}
