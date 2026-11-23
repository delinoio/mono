// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	otelattr "go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
)

// failingBody returns its prefix and then err, like a connection that breaks
// in the middle of a request body.
type failingBody struct {
	prefix []byte
	err    error
}

func (b *failingBody) Read(p []byte) (int, error) {
	if len(b.prefix) == 0 {
		return 0, b.err
	}
	n := copy(p, b.prefix)
	b.prefix = b.prefix[n:]
	return n, nil
}

func (b *failingBody) Close() error { return nil }

// webhookRejectionFixture runs WebhookHandler without a database, which is
// enough for every request that the handler rejects before it applies an
// event.
type webhookRejectionFixture struct {
	handler http.Handler
	logs    *bytes.Buffer
	reader  *sdkmetric.ManualReader
}

func newWebhookRejectionFixture(t *testing.T) *webhookRejectionFixture {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	m, err := metrics.New(provider)
	if err != nil {
		t.Fatalf("metrics.New: %v", err)
	}
	logs := &bytes.Buffer{}
	s := New(Config{
		Logger:        slog.New(slog.NewJSONHandler(logs, nil)),
		Metrics:       m,
		WebhookSecret: "test-secret",
	})
	return &webhookRejectionFixture{handler: s.WebhookHandler(), logs: logs, reader: reader}
}

// rejection returns the status and reason of the only
// billing_paddle_webhook_rejected record.
func (f *webhookRejectionFixture) rejection(t *testing.T) (status int, reason string) {
	t.Helper()
	var found int
	for _, line := range strings.Split(strings.TrimSpace(f.logs.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %v", err)
		}
		if rec["msg"] != "billing_paddle_webhook_rejected" {
			continue
		}
		found++
		code, _ := rec["status"].(float64)
		status = int(code)
		reason, _ = rec["reason"].(string)
	}
	if found != 1 {
		t.Fatalf("billing_paddle_webhook_rejected records = %d; want 1", found)
	}
	return status, reason
}

// webhookCount returns the value of the counter accounts_paddle_webhooks for
// the unknown event type and result.
func (f *webhookRejectionFixture) webhookCount(t *testing.T, result string) int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := f.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var total int64
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "accounts_paddle_webhooks" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("accounts_paddle_webhooks data is %T; want Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				et, _ := dp.Attributes.Value(otelattr.Key("event_type"))
				res, _ := dp.Attributes.Value(otelattr.Key("result"))
				if et.AsString() == "unknown" && res.AsString() == result {
					total += dp.Value
				}
			}
		}
	}
	return total
}

func (f *webhookRejectionFixture) serve(body io.ReadCloser) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/paddle", nil)
	req.Body = body
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func TestWebhookBodyReadFailureIs400(t *testing.T) {
	for name, readErr := range map[string]error{
		"unexpected_eof":   io.ErrUnexpectedEOF,
		"connection_reset": errors.New("connection reset by peer"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newWebhookRejectionFixture(t)
			rec := f.serve(&failingBody{prefix: []byte(`{"event_id":"evt_`), err: readErr})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d; want 400", rec.Code)
			}
			status, reason := f.rejection(t)
			if status != http.StatusBadRequest || reason != "body_read_failed" {
				t.Errorf("log status, reason = %d, %q; want 400, body_read_failed", status, reason)
			}
			if got := f.webhookCount(t, webhookInvalidPayload); got != 1 {
				t.Errorf("unknown/invalid_payload count = %d; want 1", got)
			}
		})
	}
}

func TestWebhookBodyOverLimitIs413(t *testing.T) {
	f := newWebhookRejectionFixture(t)
	rec := f.serve(io.NopCloser(bytes.NewReader(make([]byte, maxWebhookBytes+1))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d; want 413", rec.Code)
	}
	status, reason := f.rejection(t)
	if status != http.StatusRequestEntityTooLarge || reason != "body_too_large" {
		t.Errorf("log status, reason = %d, %q; want 413, body_too_large", status, reason)
	}
	if got := f.webhookCount(t, webhookInvalidPayload); got != 1 {
		t.Errorf("unknown/invalid_payload count = %d; want 1", got)
	}
}

// A body of exactly maxWebhookBytes is read completely, so the request
// reaches the signature check instead of a body rejection.
func TestWebhookBodyAtLimitReachesSignatureCheck(t *testing.T) {
	f := newWebhookRejectionFixture(t)
	rec := f.serve(io.NopCloser(bytes.NewReader(make([]byte, maxWebhookBytes))))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d; want 401", rec.Code)
	}
	status, reason := f.rejection(t)
	if status != http.StatusUnauthorized || reason != "signature_missing" {
		t.Errorf("log status, reason = %d, %q; want 401, signature_missing", status, reason)
	}
	if got := f.webhookCount(t, webhookInvalidSignature); got != 1 {
		t.Errorf("unknown/invalid_signature count = %d; want 1", got)
	}
}
