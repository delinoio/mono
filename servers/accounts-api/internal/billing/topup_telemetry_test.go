// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgconn"
	otelattr "go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

type topUpMeasurement struct {
	name  string
	kind  TopUpKind
	value string
}

type topUpLog struct {
	Message          string        `json:"msg"`
	BillingAccountID string        `json:"billing_account_id"`
	TopUpID          string        `json:"top_up_id"`
	Kind             TopUpKind     `json:"kind"`
	Reason           FailureReason `json:"reason"`
	PreviousStatus   TopUpStatus   `json:"previous_status"`
	TransactionID    string        `json:"paddle_transaction_id"`
	Amount           int64         `json:"amount_usd_micros"`
	Failures         int           `json:"consecutive_failures"`
	Outcome          string        `json:"outcome"`
	PaddleErrorCode  string        `json:"paddle_error_code"`
}

func (c *billingTelemetryCapture) wantTopUps(t *testing.T, wantCounts map[topUpMeasurement]int64, wantLogs ...topUpLog) {
	t.Helper()
	var gotLogs []topUpLog
	dec := json.NewDecoder(bytes.NewReader(c.logs.Bytes()))
	for dec.More() {
		var log topUpLog
		if err := dec.Decode(&log); err != nil {
			t.Fatal(err)
		}
		switch log.Message {
		case "billing_top_up_charge", "billing_top_up_failed", "billing_top_up_completed", "billing_auto_top_up_disabled":
			gotLogs = append(gotLogs, log)
		}
	}
	if !slices.Equal(gotLogs, wantLogs) {
		t.Errorf("top-up logs = %+v, want %+v", gotLogs, wantLogs)
	}

	var data metricdata.ResourceMetrics
	if err := c.reader.Collect(ctx(), &data); err != nil {
		t.Fatal(err)
	}
	gotCounts := map[topUpMeasurement]int64{}
	for _, scope := range data.ScopeMetrics {
		for _, metric := range scope.Metrics {
			var label otelattr.Key
			switch metric.Name {
			case "accounts_top_up_results":
				label = "result"
			case "accounts_top_up_attempts":
				label = "outcome"
			case "accounts_auto_top_up_disabled":
			default:
				continue
			}
			sum, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("%s data is %T, want Sum[int64]", metric.Name, metric.Data)
			}
			for _, point := range sum.DataPoints {
				kind, _ := point.Attributes.Value("kind")
				value, _ := point.Attributes.Value(label)
				gotCounts[topUpMeasurement{metric.Name, TopUpKind(kind.AsString()), value.AsString()}] += point.Value
			}
		}
	}
	if !maps.Equal(gotCounts, wantCounts) {
		t.Errorf("top-up counters = %v, want %v", gotCounts, wantCounts)
	}
}

func TestAutoTopUpTimeoutObservationsRequireCommit(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	telemetry := f.captureBillingTelemetry()
	acct := f.account()
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 25*usd)

	const id = "01900000-0000-7000-8000-0000000000e1"
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind, amount_usd_micros,
			status, create_time, update_time) VALUES ($1, $2, 'auto', $3, 'charged', $4, $4)`,
		id, acct, 25*usd, f.clock.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE credit_settings SET auto_top_up_consecutive_failures = 1;
		ALTER TABLE billing_notifications ADD CONSTRAINT reject_shutoff_notice
		CHECK (kind <> 'auto_top_up_disabled') NOT VALID`); err != nil {
		t.Fatal(err)
	}
	f.clock.Advance(2 * TopUpTimeout)
	checkState := func(status string, enabled bool, failures int32, notices int) {
		t.Helper()
		wantStatuses(t, "timeout status", f.topUpStatuses(acct), status)
		st, err := loadSettings(ctx(), f.db.Pool, acct)
		if err != nil || st.AutoEnabled != enabled || st.ConsecutiveFailures != failures {
			t.Fatalf("automatic top-up settings = %+v, err = %v", st, err)
		}
		if got := f.count(`SELECT count(*) FROM billing_notifications`); got != notices {
			t.Fatalf("notifications = %d, want %d", got, notices)
		}
	}
	var pgErr *pgconn.PgError
	if err := f.s.TimeOutTopUps(ctx()); !errors.As(err, &pgErr) || pgErr.ConstraintName != "reject_shutoff_notice" {
		t.Fatalf("timeout error = %v, want rejected shutoff notice", err)
	}
	checkState("charged", true, 1, 0)
	telemetry.wantTopUps(t, nil)

	if _, err := f.db.Pool.Exec(ctx(), `ALTER TABLE billing_notifications DROP CONSTRAINT reject_shutoff_notice`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.s.TimeOutTopUps(ctx()); err != nil {
			t.Fatal(err)
		}
		checkState("failed:timed_out", false, 2, 2)
		telemetry.wantTopUps(t, map[topUpMeasurement]int64{
			{"accounts_top_up_results", TopUpAuto, "timed_out"}: 1,
			{"accounts_auto_top_up_disabled", "", ""}:           1,
		}, topUpLog{
			Message: "billing_top_up_failed", BillingAccountID: acct, TopUpID: id, Kind: TopUpAuto,
			Reason: FailureTimedOut, PreviousStatus: TopUpCharged, Amount: 25 * usd,
		}, topUpLog{
			Message: "billing_auto_top_up_disabled", BillingAccountID: acct, TopUpID: id, Failures: 2,
		})
	}
}

func TestTopUpCompletionObservationRequiresCommit(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		install string
		remove  string
	}{
		{
			name: "callback failure",
			install: `ALTER TABLE paddle_events ADD CONSTRAINT reject_completion
				CHECK (result <> 'applied') NOT VALID`,
			remove: `ALTER TABLE paddle_events DROP CONSTRAINT reject_completion`,
		},
		{
			name: "deferred commit failure",
			install: `CREATE FUNCTION reject_completion() RETURNS trigger LANGUAGE plpgsql
				AS $$ BEGIN RAISE EXCEPTION 'injected commit failure'; END $$;
				CREATE CONSTRAINT TRIGGER reject_completion AFTER UPDATE ON paddle_events
				DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION reject_completion()`,
			remove: `DROP TRIGGER reject_completion ON paddle_events`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			telemetry := f.captureBillingTelemetry()
			acct := f.account()
			const id = "01900000-0000-7000-8000-0000000000e2"
			if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind, amount_usd_micros,
					status, create_time, update_time, idempotency_key) VALUES ($1, $2, 'manual', $3, 'charged', $4, $4, $5)`,
				id, acct, 25*usd, f.clock.Now(), topUpKey); err != nil {
				t.Fatal(err)
			}
			f.timeOutTopUps()

			if _, err := f.db.Pool.Exec(ctx(), tc.install); err != nil {
				t.Fatal(err)
			}
			txnID := f.paddle.NewID("txn")
			body := paddletest.Event("evt_top_up_completion", paddle.EventTransactionCompleted, f.clock.Now(),
				paddletest.Transaction(paddletest.TransactionData{
					ID: txnID, Origin: paddle.OriginSubscriptionCharge, TopUpID: id, SubtotalCents: 2500,
				}))
			sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
			if code := f.deliver(body, sig); code != http.StatusInternalServerError {
				t.Fatalf("failed delivery status = %d, want 500", code)
			}
			wantStatuses(t, "after rollback", f.topUpStatuses(acct), "failed:timed_out")
			f.wantCredit(acct, 0, 0)
			if events, balance := f.eventCount(), f.balance(acct); events != 0 || balance != 0 {
				t.Fatalf("after rollback: events = %d, balance = %d", events, balance)
			}
			counts := map[topUpMeasurement]int64{{"accounts_top_up_results", TopUpManual, "timed_out"}: 1}
			timeoutLog := topUpLog{
				Message: "billing_top_up_failed", BillingAccountID: acct, TopUpID: id, Kind: TopUpManual,
				Reason: FailureTimedOut, PreviousStatus: TopUpCharged, Amount: 25 * usd,
			}
			telemetry.wantTopUps(t, counts, timeoutLog)

			if _, err := f.db.Pool.Exec(ctx(), tc.remove); err != nil {
				t.Fatal(err)
			}
			counts[topUpMeasurement{"accounts_top_up_results", TopUpManual, "completed"}] = 1
			for range 2 {
				if code := f.deliver(body, sig); code != http.StatusOK {
					t.Fatalf("retry status = %d, want 200", code)
				}
				wantStatuses(t, "after commit", f.topUpStatuses(acct), "completed")
				f.wantCredit(acct, 1, 1)
				if events, balance := f.eventCount(), f.balance(acct); events != 1 || balance != 25*usd {
					t.Fatalf("after commit: events = %d, balance = %d", events, balance)
				}
				telemetry.wantTopUps(t, counts, timeoutLog, topUpLog{
					Message: "billing_top_up_completed", BillingAccountID: acct, TopUpID: id, Kind: TopUpManual,
					PreviousStatus: TopUpFailed, TransactionID: txnID,
				})
			}
		})
	}
}

func TestTopUpAttemptSurvivesOutcomeRollback(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	telemetry := f.captureBillingTelemetry()
	acct := f.account()
	f.activate(acct)
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})
	if _, err := f.db.Pool.Exec(ctx(), `ALTER TABLE top_ups ADD CONSTRAINT reject_failure CHECK (status <> 'failed') NOT VALID`); err != nil {
		t.Fatal(err)
	}

	var pgErr *pgconn.PgError
	if err := f.manualTopUp(acct, topUpKey); !errors.As(err, &pgErr) || pgErr.ConstraintName != "reject_failure" {
		t.Fatalf("charge error = %v, want the outcome write error", err)
	}
	wantStatuses(t, "after the outcome rollback", f.topUpStatuses(acct), "charging")
	charges := f.paddle.Charges()
	if len(charges) != 1 {
		t.Fatalf("Paddle calls = %d, want 1", len(charges))
	}
	attemptLog := func(id string) topUpLog {
		return topUpLog{
			Message: "billing_top_up_charge", BillingAccountID: acct, TopUpID: id, Kind: TopUpManual,
			Amount: 25 * usd, Outcome: "rejected", PaddleErrorCode: paddle.CodePaymentDeclined,
		}
	}
	attempt := topUpMeasurement{"accounts_top_up_attempts", TopUpManual, "rejected"}
	firstLog := attemptLog(paddletest.ChargeTopUpID(charges[0]))
	telemetry.wantTopUps(t, map[topUpMeasurement]int64{attempt: 1}, firstLog)

	if _, err := f.db.Pool.Exec(ctx(), `ALTER TABLE top_ups DROP CONSTRAINT reject_failure`); err != nil {
		t.Fatal(err)
	}
	const anotherKey = "01900000-0000-7000-8000-0000000000e3"
	f.clock.Advance(time.Second)
	wantConnectCode(t, "charge with committed outcome", f.manualTopUp(acct, anotherKey), connect.CodeFailedPrecondition)
	wantConnectCode(t, "replay with committed outcome", f.manualTopUp(acct, anotherKey), connect.CodeFailedPrecondition)
	charges = f.paddle.Charges()
	if len(charges) != 2 {
		t.Fatalf("Paddle calls = %d, want 2", len(charges))
	}
	id := paddletest.ChargeTopUpID(charges[1])
	telemetry.wantTopUps(t, map[topUpMeasurement]int64{
		attempt: 2,
		{"accounts_top_up_results", TopUpManual, "failed"}: 1,
	}, firstLog, attemptLog(id), topUpLog{
		Message: "billing_top_up_failed", BillingAccountID: acct, TopUpID: id, Kind: TopUpManual,
		Reason: FailureDeclined, PreviousStatus: TopUpCharging, Amount: 25 * usd,
	})
}
