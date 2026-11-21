// SPDX-License-Identifier: FSL-1.1-ALv2

package metrics_test

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
)

func TestServeAndShutdown(t *testing.T) {
	m := metrics.New()
	m.Webhook("transaction.completed", "applied")
	m.TopUpAttempt("auto", "rejected")
	m.TopUpResult("manual", "completed")
	m.Email("low_balance", "sent")
	m.CreditGrant("subscription_credit")
	m.AutoTopUpDisabled()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- metrics.Serve(ctx, listener, m, slog.New(slog.NewJSONHandler(io.Discard, nil))) }()

	resp, err := http.Get("http://" + listener.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{
		`accounts_paddle_webhooks_total{event_type="transaction.completed",result="applied"} 1`,
		`accounts_top_up_attempts_total{kind="auto",outcome="rejected"} 1`,
		`accounts_top_up_results_total{kind="manual",result="completed"} 1`,
		`accounts_billing_emails_total{kind="low_balance",result="sent"} 1`,
		`accounts_credit_grants_total{source="subscription_credit"} 1`,
		`accounts_auto_top_up_disabled_total 1`,
		`go_goroutines`,
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("metrics lack %q", want)
		}
	}
	resp, err = http.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("/healthz on the metrics listener = %d, want 404", resp.StatusCode)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve = %v", err)
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
}
