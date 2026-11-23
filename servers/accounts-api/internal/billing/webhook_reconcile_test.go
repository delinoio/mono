// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"io"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// insertAccount inserts a billing account with SQL and returns its ID.
func insertAccount(t *testing.T, d *db.DB, at time.Time) string {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx(), `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time)
		VALUES ($1, 'Reconcile', $2, $2)`, id, at); err != nil {
		t.Fatal(err)
	}
	return id
}

// oldReleaseGrant writes the purchase grant the way a release without the
// pending adjustment table does: the grant row, the ledger entry and the
// balance, with no look at pending_credit_adjustments. It returns the grant ID.
func oldReleaseGrant(t *testing.T, d *db.DB, accountID, transactionID string, amount int64, at time.Time) string {
	t.Helper()
	grantID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	entryID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx(), `INSERT INTO credit_grants (credit_grant_id, billing_account_id, source, original_usd_micros,
			remaining_usd_micros, paddle_transaction_id, create_time, expire_time)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7)`,
		grantID, accountID, string(LedgerSubscriptionCredit), amount, transactionID, at, GrantExpireTime(at)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx(), `INSERT INTO ledger_entries (ledger_entry_id, billing_account_id, kind, amount_usd_micros,
			balance_after_usd_micros, create_time, paddle_transaction_id, credit_grant_id)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7)`,
		entryID, accountID, string(LedgerSubscriptionCredit), amount, at, transactionID, grantID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx(), `UPDATE billing_accounts SET balance_usd_micros = balance_usd_micros + $2
		WHERE billing_account_id = $1`, accountID, amount); err != nil {
		t.Fatal(err)
	}
	return grantID
}

func insertPending(t *testing.T, d *db.DB, id, transactionID, action string, amount int64, at time.Time) {
	t.Helper()
	if _, err := d.Pool.Exec(ctx(), `INSERT INTO pending_credit_adjustments (paddle_adjustment_id, paddle_transaction_id,
			action, amount_usd_micros, create_time) VALUES ($1, $2, $3, $4, $5)`, id, transactionID, action, amount, at); err != nil {
		t.Fatal(err)
	}
}

// TestReconcilePendingAdjustmentsAfterOldReleaseGrant replays a rollout. The
// database is at migration 00004. The new release queued an approved
// adjustment before the payment arrived, and the previous release (which does
// not drain pending rows) then created the grant. The new release, deployed
// again on the fully migrated database, reverses the adjustment once at
// startup, and neither a second run nor a repeated payment delivery reverses
// it again.
func TestReconcilePendingAdjustmentsAfterOldReleaseGrant(t *testing.T) {
	d, err := db.Open(ctx(), dbtest.NewURLAtVersion(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	now := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	acct := insertAccount(t, d, now)
	// The adjustment of another purchase has no grant and stays pending.
	insertPending(t, d, "adj_old_1", "txn_buy", string(paddle.AdjustmentActionRefund), 4*usd, now)
	insertPending(t, d, "adj_waiting", "txn_never_paid", string(paddle.AdjustmentActionRefund), 1*usd, now)
	grantID := oldReleaseGrant(t, d, acct, "txn_buy", 10*usd, now.Add(time.Minute))
	if err := d.Migrate(ctx(), slog.New(slog.NewJSONHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}

	f := newFixtureOn(t, d)
	f.clock.Set(now.Add(time.Hour))
	if f.pendingCount() != 2 || f.refundCount() != 0 {
		t.Fatalf("before the worker: pending %d, refunds %d", f.pendingCount(), f.refundCount())
	}
	// The worker task of the new release runs at startup.
	f.s.RunOnce(ctx())
	want := []wantEntry{{LedgerSubscriptionCredit, 10 * usd, 10 * usd}, {LedgerRefund, -4 * usd, 6 * usd}}
	f.wantLedger(acct, want...)
	if f.remaining(grantID) != 6*usd || f.refundCount() != 1 || f.pendingCount() != 1 {
		t.Fatalf("after the worker: remaining %d, refunds %d, pending %d", f.remaining(grantID), f.refundCount(), f.pendingCount())
	}
	if n := f.count(`SELECT count(*) FROM pending_credit_adjustments WHERE paddle_adjustment_id = 'adj_waiting'`); n != 1 {
		t.Fatal("the pending row of a purchase without a grant was removed")
	}

	// A restart runs the task again, and Paddle delivers the payment (twice
	// with one event ID) and the adjustment again. None reverses it again.
	f.s.RunOnce(ctx())
	pay := paddletest.Event("evt_pay_dup", paddle.EventTransactionCompleted, f.clock.Now(),
		purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000))
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), pay)
	for range 2 {
		if code := f.deliver(pay, sig); code != http.StatusOK {
			t.Fatalf("payment status %d", code)
		}
	}
	if code := f.webhook(paddle.EventAdjustmentUpdated,
		paddletest.Adjustment("adj_old_1", "refund", "approved", "txn_buy", 400)); code != http.StatusOK {
		t.Fatalf("adjustment status %d", code)
	}
	f.s.RunOnce(ctx())
	f.wantLedger(acct, want...)
	if f.refundCount() != 1 || f.pendingCount() != 1 {
		t.Fatalf("after the repeats: refunds %d, pending %d", f.refundCount(), f.pendingCount())
	}
}

// TestPaymentDeliveryWithExistingGrantAppliesPending checks that a payment
// event with a new event ID, for a purchase whose grant exists already,
// applies the pending adjustments of the purchase, and that a replay of the
// same event ID changes nothing.
func TestPaymentDeliveryWithExistingGrantAppliesPending(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	if code := f.webhook(paddle.EventAdjustmentCreated,
		paddletest.Adjustment("adj_1", "chargeback", "approved", "txn_buy", 300)); code != http.StatusOK {
		t.Fatal(code)
	}
	oldReleaseGrant(t, f.db, acct, "txn_buy", 10*usd, f.clock.Now())
	if f.pendingCount() != 1 {
		t.Fatalf("pending rows = %d", f.pendingCount())
	}
	pay := paddletest.Event("evt_pay_1", paddle.EventTransactionCompleted, f.clock.Now(),
		purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000))
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), pay)
	for range 2 {
		if code := f.deliver(pay, sig); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	}
	// A second notification of the payment under a new event ID.
	if code := f.webhook(paddle.EventTransactionCompleted, purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000)); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerRefund, -3 * usd, 7 * usd},
	)
	if f.pendingCount() != 0 || f.refundCount() != 1 {
		t.Fatalf("pending %d, refunds %d", f.pendingCount(), f.refundCount())
	}
}

// TestReconcilePendingAdjustmentsIsExactlyOnce checks the cases that keep the
// effect single: a row whose adjustment credit_refunds already has is only
// deleted, the cap of the purchase applies in arrival order, and a deleted
// billing account keeps its rows.
func TestReconcilePendingAdjustmentsIsExactlyOnce(t *testing.T) {
	f := newFixture(t)
	now := f.clock.Now()
	acct := f.account()
	oldReleaseGrant(t, f.db, acct, "txn_buy", 10*usd, now)
	insertPending(t, f.db, "adj_a", "txn_buy", "refund", 6*usd, now)
	insertPending(t, f.db, "adj_b", "txn_buy", "chargeback", 6*usd, now.Add(time.Second))
	if err := f.s.ReconcilePendingAdjustments(ctx()); err != nil {
		t.Fatal(err)
	}
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerRefund, -6 * usd, 4 * usd},
		wantEntry{LedgerRefund, -4 * usd, 0},
	)
	if f.pendingCount() != 0 || f.refundCount() != 2 {
		t.Fatalf("pending %d, refunds %d", f.pendingCount(), f.refundCount())
	}

	// A pending row left behind although credit_refunds applied it (for
	// example a copy that an older release wrote after the webhook) is
	// deleted without a second reversal.
	acct2 := f.account()
	oldReleaseGrant(t, f.db, acct2, "txn_buy2", 10*usd, now)
	if code := f.webhook(paddle.EventAdjustmentCreated,
		paddletest.Adjustment("adj_c", "refund", "approved", "txn_buy2", 200)); code != http.StatusOK {
		t.Fatal(code)
	}
	insertPending(t, f.db, "adj_c", "txn_buy2", "refund", 2*usd, now)
	if err := f.s.ReconcilePendingAdjustments(ctx()); err != nil {
		t.Fatal(err)
	}
	f.wantLedger(acct2,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerRefund, -2 * usd, 8 * usd},
	)
	if f.pendingCount() != 0 || f.refundCount() != 3 {
		t.Fatalf("pending %d, refunds %d", f.pendingCount(), f.refundCount())
	}

	// A deleted billing account keeps its pending row and its balance.
	acct3 := f.account()
	oldReleaseGrant(t, f.db, acct3, "txn_buy3", 10*usd, now)
	insertPending(t, f.db, "adj_d", "txn_buy3", "refund", 5*usd, now)
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE billing_accounts SET delete_time = $2 WHERE billing_account_id = $1`, acct3, now); err != nil {
		t.Fatal(err)
	}
	if err := f.s.ReconcilePendingAdjustments(ctx()); err != nil {
		t.Fatal(err)
	}
	if f.pendingCount() != 1 || f.refundCount() != 3 || f.balance(acct3) != 10*usd {
		t.Fatalf("deleted account: pending %d, refunds %d, balance %d", f.pendingCount(), f.refundCount(), f.balance(acct3))
	}
}
