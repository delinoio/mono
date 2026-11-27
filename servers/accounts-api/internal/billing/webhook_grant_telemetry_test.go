// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/delinoio/mono/servers/accounts-api/internal/metrics"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

type grantLog struct {
	Message          string     `json:"msg"`
	BillingAccountID string     `json:"billing_account_id"`
	GrantID          string     `json:"credit_grant_id"`
	Source           LedgerKind `json:"source"`
	TransactionID    string     `json:"paddle_transaction_id"`
	Amount           int64      `json:"amount_usd_micros"`
	DebtPaid         int64      `json:"debt_paid_usd_micros"`
	Balance          int64      `json:"balance_after_usd_micros"`
}

type grantTelemetryCapture struct {
	logs   bytes.Buffer
	reader *sdkmetric.ManualReader
}

func (f *fixture) captureGrantTelemetry() *grantTelemetryCapture {
	f.t.Helper()
	capture := &grantTelemetryCapture{reader: sdkmetric.NewManualReader()}
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(capture.reader))
	f.t.Cleanup(func() { _ = provider.Shutdown(ctx()) })
	m, err := metrics.New(provider)
	if err != nil {
		f.t.Fatal(err)
	}
	f.s.logger = slog.New(slog.NewJSONHandler(&capture.logs, nil))
	f.s.metrics = m
	return capture
}

func (c *grantTelemetryCapture) want(t *testing.T, entries ...LedgerEntry) {
	t.Helper()
	var wantLogs, gotLogs []grantLog
	wantCounts := map[LedgerKind]int64{}
	for _, e := range entries {
		if e.CreditGrantID == nil || e.PaddleTransactionID == nil {
			t.Fatalf("grant ledger entry missing grant or transaction ID: %+v", e)
		}
		wantLogs = append(wantLogs, grantLog{
			Message: "billing_credit_granted", BillingAccountID: e.BillingAccountID,
			GrantID: *e.CreditGrantID, Source: e.Kind, TransactionID: *e.PaddleTransactionID,
			Amount: e.Amount, Balance: e.BalanceAfter,
		})
		wantCounts[e.Kind]++
	}
	dec := json.NewDecoder(bytes.NewReader(c.logs.Bytes()))
	for dec.More() {
		var log grantLog
		if err := dec.Decode(&log); err != nil {
			t.Fatalf("decode billing log: %v", err)
		}
		if log.Message == "billing_credit_granted" {
			gotLogs = append(gotLogs, log)
		}
	}
	if !slices.Equal(gotLogs, wantLogs) {
		t.Errorf("grant logs = %+v, want %+v", gotLogs, wantLogs)
	}

	var data metricdata.ResourceMetrics
	if err := c.reader.Collect(ctx(), &data); err != nil {
		t.Fatal(err)
	}
	gotCounts := map[LedgerKind]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != "accounts_credit_grants" {
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("grant counter data is %T, want Sum[int64]", metric.Data)
			}
			for _, point := range sum.DataPoints {
				source, _ := point.Attributes.Value("source")
				gotCounts[LedgerKind(source.AsString())] += point.Value
			}
		}
	}
	if !maps.Equal(gotCounts, wantCounts) {
		t.Errorf("grant counter = %v, want %v", gotCounts, wantCounts)
	}
}

func TestWebhookCommitFailureDoesNotReportGrant(t *testing.T) {
	f := newFixture(t)
	telemetry := f.captureGrantTelemetry()
	acct := f.account()
	if _, err := f.db.Pool.Exec(ctx(), `CREATE FUNCTION fail_payment_commit() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER fail_payment_commit AFTER UPDATE ON paddle_events
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_payment_commit()`); err != nil {
		t.Fatal(err)
	}

	body := paddletest.Event("evt_commit", paddle.EventTransactionCompleted, f.clock.Now(),
		purchase("txn_commit", acct, LedgerSubscriptionCredit, 1000))
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	if code := f.deliver(body, sig); code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	f.wantLedger(acct)
	if grants, events, balance := f.count(`SELECT count(*) FROM credit_grants`), f.eventCount(), f.balance(acct); grants != 0 || events != 0 || balance != 0 {
		t.Fatalf("after failed commit: grants %d, events %d, balance %d", grants, events, balance)
	}
	telemetry.want(t)

	if _, err := f.db.Pool.Exec(ctx(), `DROP TRIGGER fail_payment_commit ON paddle_events`); err != nil {
		t.Fatal(err)
	}
	if code := f.deliver(body, sig); code != http.StatusOK {
		t.Fatalf("retry status %d, want 200", code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	if grants, events := f.count(`SELECT count(*) FROM credit_grants`), f.eventCount(); grants != 1 || events != 1 {
		t.Fatalf("after committed retry: grants %d, events %d", grants, events)
	}
	telemetry.want(t, f.ledger(acct)[0])
}
