// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
)

// TestReconcilePendingAdjustmentExpiresGrantDueWhileWaitingForLocks checks
// that the reconcile task reads the clock after it holds the purchase lock and
// the billing account lock, like the refund webhook: a grant that expired
// while the task waited is expired first and is not consumed, and the
// pending row is deleted once.
func TestReconcilePendingAdjustmentExpiresGrantDueWhileWaitingForLocks(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	if code := f.webhook(paddle.EventTransactionCompleted, purchase("txn_rec", acct, LedgerManualTopUp, 2500)); code != http.StatusOK {
		t.Fatal(code)
	}
	expire := f.grantExpireTime("txn_rec")
	f.clock.Set(expire.Add(-time.Minute))
	// A grant that another release created leaves the pending row of its
	// purchase behind.
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO pending_credit_adjustments (paddle_adjustment_id, paddle_transaction_id,
		action, amount_usd_micros, create_time) VALUES ('adj_rec', 'txn_rec', 'refund', $1, $2)`,
		25*usd, f.clock.Now()); err != nil {
		t.Fatal(err)
	}

	release := f.holdAccountLock(acct)
	result := make(chan error, 1)
	go func() { result <- f.s.ReconcilePendingAdjustments(ctx()) }()
	f.waitForAccountLockWait()
	f.clock.Set(expire.Add(time.Minute))
	release()
	if err := <-result; err != nil {
		t.Fatal(err)
	}

	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
		wantEntry{LedgerRefund, -25 * usd, -25 * usd},
	)
	if f.pendingCount() != 0 || f.refundCount() != 1 {
		t.Fatalf("pending rows %d, credit_refunds rows %d, want 0 and 1", f.pendingCount(), f.refundCount())
	}
	// Another run finds nothing to apply.
	if err := f.s.ReconcilePendingAdjustments(ctx()); err != nil {
		t.Fatal(err)
	}
	if f.refundCount() != 1 || f.balance(acct) != -25*usd {
		t.Fatalf("credit_refunds rows %d, balance %d after a second run", f.refundCount(), f.balance(acct))
	}
}
