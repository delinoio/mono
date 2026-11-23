// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/config"
)

// setRunEnv sets the configuration that lets run start without external
// services. The database pool opens lazily and RUN_MIGRATIONS=false skips the
// migrations, so the unreachable database (port 1 refuses connections) is
// contacted only by billing work, which these tests must never reach.
//
// LOG_LEVEL is set explicitly because run builds its logger from it and the
// billing_worker_started event is logged at INFO. Without it the test would
// inherit the parent's LOG_LEVEL, and a warn or error level would drop that
// event and let the "worker did not start" assertion pass vacuously.
func setRunEnv(t *testing.T, metricsAddr string) {
	t.Helper()
	env := map[string]string{
		"DATABASE_URL":                 "postgres://accounts@127.0.0.1:1/accounts?connect_timeout=1",
		"RUN_MIGRATIONS":               "false",
		"ADDR":                         "127.0.0.1:0",
		"METRICS_ADDR":                 metricsAddr,
		config.EnvLogLevel:             "info",
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

// captureRunOutput redirects os.Stdout, where run writes its logs, and
// restores it and the default logger when the test ends. The returned function
// stops the capture and returns the output.
func captureRunOutput(t *testing.T) func() string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	original := os.Stdout
	originalLogger := slog.Default()
	os.Stdout = writer
	var buf bytes.Buffer
	copied := make(chan struct{})
	go func() {
		defer close(copied)
		_, _ = io.Copy(&buf, reader)
	}()
	stopped := false
	stop := func() string {
		if !stopped {
			stopped = true
			os.Stdout = original
			slog.SetDefault(originalLogger)
			_ = writer.Close()
			<-copied
			_ = reader.Close()
		}
		return buf.String()
	}
	t.Cleanup(func() { stop() })
	return stop
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
			stopCapture := captureRunOutput(t)

			err := run()
			if err == nil {
				t.Fatal("run succeeded; want the metrics listen error")
			}
			var opErr *net.OpError
			if !errors.As(err, &opErr) || opErr.Op != "listen" {
				t.Fatalf("run error = %v; want a listen error", err)
			}
			// A worker that was started before the bind logs asynchronously
			// right after it starts, so give it time to do so.
			time.Sleep(300 * time.Millisecond)
			if output := stopCapture(); strings.Contains(output, "billing_worker_started") {
				t.Fatalf("billing worker started although run failed to bind METRICS_ADDR; output: %s", output)
			}
		})
	}
}
