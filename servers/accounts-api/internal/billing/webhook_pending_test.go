// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func (f *fixture) count(query string, args ...any) int {
	f.t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(ctx(), query, args...).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

func (f *fixture) pendingCount() int {
	return f.count(`SELECT count(*) FROM pending_credit_adjustments`)
}

func (f *fixture) refundCount() int {
	return f.count(`SELECT count(*) FROM credit_refunds`)
}

// purchase returns the event data of a completed purchase. A subscription
// payment and a one-time charge both grant credit; kind selects which.
func purchase(txnID, acct string, kind LedgerKind, cents int64) map[string]any {
	if kind == LedgerSubscriptionCredit {
		return paddletest.Transaction(paddletest.TransactionData{
			ID: txnID, Origin: paddle.OriginWeb, BillingAccountID: acct, PriceID: priceID, SubtotalCents: cents,
		})
	}
	return paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, Origin: paddle.OriginSubscriptionCharge, BillingAccountID: acct, SubtotalCents: cents,
	})
}

// TestAdjustmentBeforePurchaseMatchesGrantFirst checks that an approved
// adjustment has the same final ledger effect in either delivery order, for a
// subscription credit and for a top-up.
func TestAdjustmentBeforePurchaseMatchesGrantFirst(t *testing.T) {
	cases := []struct {
		name  string
		kind  LedgerKind
		cents int64
		want  []wantEntry
	}{
		{"subscription credit", LedgerSubscriptionCredit, 1000, []wantEntry{
			{LedgerSubscriptionCredit, 10 * usd, 10 * usd}, {LedgerRefund, -4 * usd, 6 * usd}}},
		{"top-up", LedgerManualTopUp, 2500, []wantEntry{
			{LedgerManualTopUp, 25 * usd, 25 * usd}, {LedgerRefund, -4 * usd, 21 * usd}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			control, late := f.account(), f.account()

			f.webhook(paddle.EventTransactionCompleted, purchase("txn_control", control, tc.kind, tc.cents))
			f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_control", "refund", "approved", "txn_control", 400))

			// The adjustment arrives first. Its notification is acknowledged,
			// remembered and has no balance effect yet.
			if code := f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_late", "refund", "approved", "txn_late", 400)); code != http.StatusOK {
				t.Fatal(code)
			}
			if f.pendingCount() != 1 || f.refundCount() != 1 {
				t.Fatalf("pending %d, refunds %d", f.pendingCount(), f.refundCount())
			}
			f.wantLedger(late)
			// Paddle repeats the adjustment, and sends it as updated too.
			f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_late", "refund", "approved", "txn_late", 400))
			f.webhook(paddle.EventAdjustmentUpdated, paddletest.Adjustment("adj_late", "refund", "approved", "txn_late", 400))
			if f.pendingCount() != 1 {
				t.Fatalf("pending rows = %d, want 1", f.pendingCount())
			}

			data := purchase("txn_late", late, tc.kind, tc.cents)
			for range 3 {
				f.webhook(paddle.EventTransactionCompleted, data)
			}
			// Adjustments that follow the grant are no-ops.
			f.webhook(paddle.EventAdjustmentUpdated, paddletest.Adjustment("adj_late", "refund", "approved", "txn_late", 400))

			f.wantLedger(control, tc.want...)
			f.wantLedger(late, tc.want...)
			if f.balance(control) != f.balance(late) {
				t.Fatalf("balances differ: %d and %d", f.balance(control), f.balance(late))
			}
			if f.pendingCount() != 0 || f.refundCount() != 2 {
				t.Fatalf("pending %d, refunds %d, want 0 and 2", f.pendingCount(), f.refundCount())
			}
		})
	}
}

// TestPendingAdjustmentsKeepTheCapAndAccumulate checks that the cap of the
// purchase applies to pending adjustments in arrival order, with the credit
// that usage already used turning into debt.
func TestPendingAdjustmentsKeepTheCapAndAccumulate(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_a", "refund", "approved", "txn_buy", 600))
	f.clock.Advance(1)
	f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_b", "chargeback", "approved", "txn_buy", 1000))
	f.webhook(paddle.EventTransactionCompleted, purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000))
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerRefund, -6 * usd, 4 * usd},
		wantEntry{LedgerRefund, -4 * usd, 0},
	)
	if f.pendingCount() != 0 {
		t.Fatalf("pending rows = %d", f.pendingCount())
	}
}

// TestPendingAdjustmentWithoutGrantHasNoEffect checks that a purchase that
// never creates a grant is not affected, and that adjustments that are not
// approved, or that belong to another purchase, are not applied.
func TestPendingAdjustmentWithoutGrantHasNoEffect(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_wait", "refund", "pending_approval", "txn_other", 500))
	if f.pendingCount() != 0 {
		t.Fatalf("an unapproved adjustment was stored")
	}
	f.webhook(paddle.EventAdjustmentUpdated, paddletest.Adjustment("adj_other", "refund", "approved", "txn_other", 500))
	if f.pendingCount() != 1 {
		t.Fatalf("pending rows = %d, want 1", f.pendingCount())
	}
	f.webhook(paddle.EventTransactionCompleted, purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000))
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	if f.pendingCount() != 1 || f.refundCount() != 0 {
		t.Fatalf("pending %d, refunds %d", f.pendingCount(), f.refundCount())
	}
	// A bad signature never stores or applies anything.
	body := paddletest.Event("evt_forged", paddle.EventAdjustmentCreated, f.clock.Now(),
		paddletest.Adjustment("adj_forged", "refund", "approved", "txn_buy", 1000))
	if code := f.deliver(body, paddle.SignatureHeaderValue("wrong", f.clock.Now(), body)); code != http.StatusUnauthorized {
		t.Fatal(code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	if f.pendingCount() != 1 {
		t.Fatalf("pending rows = %d", f.pendingCount())
	}
}

// TestOverlappingAdjustmentAndPurchaseApplyOnce delivers the purchase and its
// adjustment at the same time. Whatever the interleaving, the purchase has
// one grant, the adjustment removes credit once and no pending row is left.
func TestOverlappingAdjustmentAndPurchaseApplyOnce(t *testing.T) {
	f := newFixture(t)
	for i := range 12 {
		acct := f.account()
		txn := fmt.Sprintf("txn_race_%d", i)
		adj := fmt.Sprintf("adj_race_%d", i)
		pay := purchase(txn, acct, LedgerSubscriptionCredit, 1000)
		refund := paddletest.Adjustment(adj, "refund", "approved", txn, 300)
		var wg sync.WaitGroup
		codes := make(chan int, 4)
		// Two notifications of the adjustment and two of the payment.
		for _, send := range []func(){
			func() { codes <- f.webhook(paddle.EventAdjustmentCreated, refund) },
			func() { codes <- f.webhook(paddle.EventAdjustmentUpdated, refund) },
			func() { codes <- f.webhook(paddle.EventTransactionCompleted, pay) },
			func() { codes <- f.webhook(paddle.EventTransactionCompleted, pay) },
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				send()
			}()
		}
		wg.Wait()
		close(codes)
		for code := range codes {
			if code != http.StatusOK {
				t.Fatalf("round %d: status %d", i, code)
			}
		}
		f.wantLedger(acct,
			wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
			wantEntry{LedgerRefund, -3 * usd, 7 * usd},
		)
		if n := f.count(`SELECT count(*) FROM credit_grants WHERE paddle_transaction_id = $1`, txn); n != 1 {
			t.Fatalf("round %d: %d grants", i, n)
		}
		if n := f.count(`SELECT count(*) FROM pending_credit_adjustments WHERE paddle_transaction_id = $1`, txn); n != 0 {
			t.Fatalf("round %d: %d pending rows", i, n)
		}
	}
}

// TestPendingAdjustmentFailureRollsBackTheGrant makes the refund write fail
// and checks that the grant, the refund, the ledger and the event record roll
// back together, and that Paddle's retry commits one result.
func TestPendingAdjustmentFailureRollsBackTheGrant(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_1", "refund", "approved", "txn_buy", 400))

	if _, err := f.db.Pool.Exec(ctx(), `CREATE FUNCTION fail_refund() RETURNS trigger LANGUAGE plpgsql
		AS $$ BEGIN RAISE EXCEPTION 'injected refund failure'; END $$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx(), `CREATE TRIGGER fail_refund BEFORE INSERT ON credit_refunds
		FOR EACH ROW EXECUTE FUNCTION fail_refund()`); err != nil {
		t.Fatal(err)
	}
	events := f.eventCount()
	body := paddletest.Event("evt_pay", paddle.EventTransactionCompleted, f.clock.Now(),
		purchase("txn_buy", acct, LedgerSubscriptionCredit, 1000))
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	if code := f.deliver(body, sig); code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", code)
	}
	if n := f.count(`SELECT count(*) FROM credit_grants`); n != 0 {
		t.Fatalf("%d grants after the failure", n)
	}
	f.wantLedger(acct)
	if f.balance(acct) != 0 || f.pendingCount() != 1 || f.refundCount() != 0 || f.eventCount() != events {
		t.Fatalf("state changed: balance %d, pending %d, refunds %d, events %d (was %d)",
			f.balance(acct), f.pendingCount(), f.refundCount(), f.eventCount(), events)
	}

	if _, err := f.db.Pool.Exec(ctx(), `DROP TRIGGER fail_refund ON credit_refunds`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code := f.deliver(body, sig); code != http.StatusOK {
			t.Fatalf("retry status %d", code)
		}
	}
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerRefund, -4 * usd, 6 * usd},
	)
	if f.pendingCount() != 0 || f.refundCount() != 1 {
		t.Fatalf("pending %d, refunds %d", f.pendingCount(), f.refundCount())
	}
}
