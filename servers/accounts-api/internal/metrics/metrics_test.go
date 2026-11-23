// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
	"github.com/delinoio/mono/servers/internal/telemetry"
)

const (
	// requestTimeout bounds each test request, including the body read.
	requestTimeout = 5 * time.Second
	// serveStopTimeout bounds the wait for Serve to return after cancel. It is
	// larger than the 5 s drain limit inside Serve so a slow runner still sees
	// Serve's own result instead of a test-side timeout.
	serveStopTimeout = 15 * time.Second
)

// startServer serves handler on a new loopback listener and registers a
// cleanup that cancels the server, waits for Serve to return and reports its
// error. t.Cleanup runs on t.Fatal and early exits too, so a failed request
// can no longer leave the server running. Register it after any cleanup that
// must outlive the server, because cleanups run in last-in-first-out order.
func startServer(t *testing.T, handler http.Handler) net.Listener {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- metrics.Serve(ctx, listener, handler, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Serve = %v", err)
			}
		case <-time.After(serveStopTimeout):
			t.Errorf("Serve did not return within %v after cancel", serveStopTimeout)
		}
	})
	return listener
}

func TestServeAndShutdown(t *testing.T) {
	tel, err := telemetry.New(context.Background(), telemetry.Options{
		ServiceName: "accounts-api",
		Stdout:      io.Discard,
		LookupEnv:   func(string) (string, bool) { return "", false },
	})
	if err != nil {
		t.Fatal(err)
	}
	// Registered before the server cleanup so telemetry shuts down after Serve.
	t.Cleanup(func() {
		if err := tel.Shutdown(context.Background()); err != nil {
			t.Errorf("telemetry shutdown: %v", err)
		}
	})
	m, err := metrics.New(tel.MeterProvider)
	if err != nil {
		t.Fatal(err)
	}
	m.Webhook("transaction.completed", "applied")
	m.TopUpAttempt("auto", "rejected")
	m.TopUpResult("manual", "completed")
	m.Email("low_balance", "sent")
	m.CreditGrant("subscription_credit")
	m.AutoTopUpDisabled()
	m.UsageReport("ownsand", "charged")
	m.StatusChange("blocked")
	m.LinkedOrganizations("suspended", 3)

	listener := startServer(t, tel.MetricsHandler(nil))
	client := &http.Client{Timeout: requestTimeout}

	resp, err := client.Get("http://" + listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`accounts_paddle_webhooks_total{event_type="transaction.completed",result="applied"} 1`,
		`accounts_top_up_attempts_total{kind="auto",outcome="rejected"} 1`,
		`accounts_top_up_results_total{kind="manual",result="completed"} 1`,
		`accounts_billing_emails_total{kind="low_balance",result="sent"} 1`,
		`accounts_credit_grants_total{source="subscription_credit"} 1`,
		`accounts_auto_top_up_disabled_total 1`,
		`accounts_usage_reports_total{result="charged",service="ownsand"} 1`,
		`accounts_billing_status_changes_total{status="blocked"} 1`,
		`accounts_linked_organizations{status="suspended"} 3`,
		`go_goroutine_count`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	resp, err = client.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/healthz on the metrics listener = %d, want 404", resp.StatusCode)
	}
}

type failingTransport struct{ err error }

func (f failingTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }

// TestServerStopsAfterFailedRequest proves that a request failure that ends
// the test early, as t.Fatal does, still shuts the metrics server down.
func TestServerStopsAfterFailedRequest(t *testing.T) {
	errInjected := errors.New("injected transport failure")
	var listener net.Listener
	// The subtest leaves through the same runtime.Goexit path as t.Fatal, but
	// t.Skip keeps the exit expected so this test can inspect the cleanup result.
	t.Run("early exit", func(t *testing.T) {
		listener = startServer(t, http.NotFoundHandler())
		client := &http.Client{Timeout: requestTimeout, Transport: failingTransport{err: errInjected}}
		resp, err := client.Get("http://" + listener.Addr().String() + "/metrics")
		if err == nil {
			resp.Body.Close()
		}
		if !errors.Is(err, errInjected) {
			t.Fatalf("request error does not wrap the injected failure: %T", err)
		}
		t.Skip("simulated early exit after a failed request")
	})
	if listener == nil {
		t.Fatal("subtest did not start the server")
	}
	// Serve closes the listener once it has shut down. The deadline turns a
	// server that is still running into a failure instead of a hang.
	if tcp, ok := listener.(*net.TCPListener); ok {
		_ = tcp.SetDeadline(time.Now().Add(serveStopTimeout))
	}
	conn, err := listener.Accept()
	if err == nil {
		conn.Close()
		t.Fatal("listener still accepts connections after the subtest cleanup")
	}
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener Accept failed for a reason other than closure: %T", err)
	}
}

func TestNilMetricsRecordNothing(t *testing.T) {
	var m *metrics.Metrics
	m.Webhook("a", "b")
	m.TopUpAttempt("a", "b")
	m.TopUpResult("a", "b")
	m.Email("a", "b")
	m.CreditGrant("a")
	m.AutoTopUpDisabled()
	m.UsageReport("a", "b")
	m.StatusChange("a")
	m.LinkedOrganizations("a", 1)
}
