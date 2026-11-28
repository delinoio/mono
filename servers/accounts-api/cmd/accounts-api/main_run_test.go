// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"errors"
	"log"
	"log/slog"
	"net"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/config"
)

// setRunEnv sets the configuration that lets run start without external
// services. The database pool opens lazily and RUN_MIGRATIONS=false skips the
// migrations. The fake dispatcher keeps billing work from contacting the
// database.
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
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
}

// TestRunFailsOnMetricsBindErrorBeforeStartingWorker verifies that a metrics
// listener that cannot be bound fails run before the billing worker starts, so
// no Paddle call can begin after run has returned.
func TestRunFailsOnMetricsBindErrorBeforeStartingWorker(t *testing.T) {
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
		})
	}
}
