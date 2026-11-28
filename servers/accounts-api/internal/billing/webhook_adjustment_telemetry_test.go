// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

const (
	refundMessage         = "billing_credit_refunded"
	reconciliationMessage = "billing_pending_adjustments_reconciled"
)

type adjustmentLog struct {
	Message       string `json:"msg"`
	Level         string `json:"level"`
	AccountID     string `json:"billing_account_id"`
	GrantID       string `json:"credit_grant_id"`
	TransactionID string `json:"paddle_transaction_id"`
	AdjustmentID  string `json:"paddle_adjustment_id"`
	Action        string `json:"action"`
	Amount        *int64 `json:"amount_usd_micros"`
	FromPending   *bool  `json:"from_pending"`
	PendingRows   *int   `json:"pending_rows"`
	Balance       *int64 `json:"balance_after_usd_micros"`
}

type adjustmentLogs struct {
	fixture *fixture
	buffer  bytes.Buffer
}

func (f *fixture) captureAdjustmentLogs() *adjustmentLogs {
	logs := &adjustmentLogs{fixture: f}
	f.s.logger = slog.New(slog.NewJSONHandler(&logs.buffer, nil))
	return logs
}

func (logs *adjustmentLogs) refund(accountID, transactionID, adjustmentID, action string, amount, balance int64, fromPending bool) adjustmentLog {
	return adjustmentLog{
		Message: refundMessage, Level: "INFO", AccountID: accountID,
		GrantID: *logs.fixture.ledger(accountID)[0].CreditGrantID, TransactionID: transactionID,
		AdjustmentID: adjustmentID, Action: action, Amount: new(amount),
		FromPending: new(fromPending), Balance: new(balance),
	}
}

func (logs *adjustmentLogs) reconciliation(accountID, transactionID string, rows int, balance int64) adjustmentLog {
	return adjustmentLog{
		Message: reconciliationMessage, Level: "INFO", AccountID: accountID,
		GrantID: *logs.fixture.ledger(accountID)[0].CreditGrantID, TransactionID: transactionID,
		PendingRows: new(rows), Balance: new(balance),
	}
}

func (logs *adjustmentLogs) want(want ...adjustmentLog) {
	t := logs.fixture.t
	t.Helper()
	var got []string
	dec := json.NewDecoder(bytes.NewReader(logs.buffer.Bytes()))
	for dec.More() {
		var record adjustmentLog
		if err := dec.Decode(&record); err != nil {
			t.Fatalf("decode billing log: %v", err)
		}
		if record.Message == refundMessage || record.Message == reconciliationMessage {
			got = append(got, adjustmentLogJSON(t, record))
		}
	}
	var expected []string
	for _, record := range want {
		expected = append(expected, adjustmentLogJSON(t, record))
	}
	if !slices.Equal(got, expected) {
		t.Fatalf("adjustment logs = %v, want %v", got, expected)
	}
}

func adjustmentLogJSON(t *testing.T, record adjustmentLog) string {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func (f *fixture) injectAdjustmentCommitFailure() {
	f.t.Helper()
	if _, err := f.db.Pool.Exec(ctx(), `CREATE FUNCTION fail_adjustment_commit() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'injected adjustment commit failure'; END $$;
		CREATE CONSTRAINT TRIGGER fail_adjustment_commit AFTER INSERT ON credit_refunds
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION fail_adjustment_commit()`); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) removeAdjustmentCommitFailure() {
	f.t.Helper()
	if _, err := f.db.Pool.Exec(ctx(), `DROP TRIGGER fail_adjustment_commit ON credit_refunds`); err != nil {
		f.t.Fatal(err)
	}
}

func TestReconcilePendingAdjustmentsCommitFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	logs := f.captureAdjustmentLogs()
	acct := f.seedPendingPurchase("txn_buy", "refund", false)
	credit := wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd}
	f.injectAdjustmentCommitFailure()

	err := f.s.ReconcilePendingAdjustments(ctx())
	if err == nil || !strings.Contains(err.Error(), "txn_buy") || !strings.Contains(err.Error(), "injected adjustment commit failure") {
		t.Fatalf("run error = %v, want the injected commit failure for txn_buy", err)
	}
	logs.want()
	f.wantLedger(acct, credit)
	if f.pendingCount() != 1 || f.refundCount() != 0 || f.balance(acct) != 10*usd {
		t.Fatalf("after rollback: pending %d, refunds %d, balance %d", f.pendingCount(), f.refundCount(), f.balance(acct))
	}

	f.removeAdjustmentCommitFailure()
	if err := f.s.ReconcilePendingAdjustments(ctx()); err != nil {
		t.Fatal(err)
	}
	f.wantLedger(acct, credit, wantEntry{LedgerRefund, -4 * usd, 6 * usd})
	if f.pendingCount() != 0 || f.refundCount() != 1 || f.balance(acct) != 6*usd {
		t.Fatalf("after retry: pending %d, refunds %d, balance %d", f.pendingCount(), f.refundCount(), f.balance(acct))
	}
	logs.want(logs.refund(acct, "txn_buy", "adj_txn_buy", "refund", 4*usd, 6*usd, true),
		logs.reconciliation(acct, "txn_buy", 1, 6*usd))
}

func TestWebhookAdjustmentCommitFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	logs := f.captureAdjustmentLogs()
	acct := f.account()
	oldReleaseGrant(t, f.db, acct, "txn_buy", 10*usd, f.clock.Now())
	credit := wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd}
	f.injectAdjustmentCommitFailure()

	body := paddletest.Event("evt_refund", paddle.EventAdjustmentCreated, f.clock.Now(),
		paddletest.Adjustment("adj_refund", "refund", "approved", "txn_buy", 400))
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	if code := f.deliver(body, sig); code != http.StatusInternalServerError {
		t.Fatalf("failed commit status = %d, want 500", code)
	}
	logs.want()
	f.wantLedger(acct, credit)
	if f.eventCount() != 0 || f.refundCount() != 0 || f.balance(acct) != 10*usd {
		t.Fatalf("after rollback: events %d, refunds %d, balance %d", f.eventCount(), f.refundCount(), f.balance(acct))
	}

	f.removeAdjustmentCommitFailure()
	for range 2 {
		if code := f.deliver(body, sig); code != http.StatusOK {
			t.Fatalf("retry or replay status = %d, want 200", code)
		}
	}
	f.wantLedger(acct, credit, wantEntry{LedgerRefund, -4 * usd, 6 * usd})
	if f.eventCount() != 1 || f.refundCount() != 1 || f.balance(acct) != 6*usd {
		t.Fatalf("after retry and replay: events %d, refunds %d, balance %d", f.eventCount(), f.refundCount(), f.balance(acct))
	}
	logs.want(logs.refund(acct, "txn_buy", "adj_refund", "refund", 4*usd, 6*usd, false))
}

func TestPendingAdjustmentPaymentDeleteFailure(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	logs := f.captureAdjustmentLogs()
	acct := f.account()
	if code := f.webhook(paddle.EventAdjustmentCreated,
		paddletest.Adjustment("adj_refund", "refund", "approved", "txn_buy", 400)); code != http.StatusOK {
		t.Fatalf("pending adjustment status = %d, want 200", code)
	}
	f.injectPendingAdjustmentFailures()
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO reconciliation_faults VALUES ('txn_buy')`); err != nil {
		t.Fatal(err)
	}
	body := paddletest.Event("evt_pay", paddle.EventTransactionCompleted, f.clock.Now(),
		purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000))
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	if code := f.deliver(body, sig); code != http.StatusInternalServerError {
		t.Fatalf("failed payment status = %d, want 500", code)
	}
	logs.want()
	f.wantLedger(acct)
	if f.eventCount() != 1 || f.pendingCount() != 1 || f.refundCount() != 0 || f.balance(acct) != 0 {
		t.Fatalf("after rollback: events %d, pending %d, refunds %d, balance %d", f.eventCount(), f.pendingCount(), f.refundCount(), f.balance(acct))
	}

	if _, err := f.db.Pool.Exec(ctx(), `DELETE FROM reconciliation_faults`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code := f.deliver(body, sig); code != http.StatusOK {
			t.Fatalf("retry or replay status = %d, want 200", code)
		}
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd}, wantEntry{LedgerRefund, -4 * usd, 6 * usd})
	if f.eventCount() != 2 || f.pendingCount() != 0 || f.refundCount() != 1 || f.balance(acct) != 6*usd {
		t.Fatalf("after retry and replay: events %d, pending %d, refunds %d, balance %d", f.eventCount(), f.pendingCount(), f.refundCount(), f.balance(acct))
	}
	logs.want(logs.refund(acct, "txn_buy", "adj_refund", "refund", 4*usd, 6*usd, true))
}
