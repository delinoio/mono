// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// holdAccountLock locks the billing account row in a transaction on its own
// connection and keeps it until the returned release function runs. The test
// cleanup releases it too, so a failed test never leaves a lock behind.
func (f *fixture) holdAccountLock(accountID string) (release func()) {
	f.t.Helper()
	locked := make(chan error, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		err := f.db.InTx(ctx(), func(tx pgx.Tx) error {
			if _, err := lockAccount(ctx(), tx, accountID); err != nil {
				return err
			}
			locked <- nil
			<-stop
			return nil
		})
		if err != nil {
			select {
			case locked <- err:
			default:
			}
		}
	}()
	if err := <-locked; err != nil {
		f.t.Fatal(err)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			close(stop)
			<-done
		})
	}
	f.t.Cleanup(release)
	return release
}

// waitForAccountLockWait returns once a database session waits for a row
// lock, which is the state of a webhook that has read its clock and waits
// for the billing account. It polls the session state without a fixed
// delay between the lock hand-over and the check, and gives up after the
// timeout.
func (f *fixture) waitForAccountLockWait() {
	f.t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx(), 20*time.Second)
	defer cancel()
	for {
		var waiting int
		if err := f.db.Pool.QueryRow(waitCtx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			f.t.Fatalf("wait for the lock wait: %v", err)
		}
		if waiting > 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

// grantExpireTime returns the expiry of the grant of a purchase.
func (f *fixture) grantExpireTime(transactionID string) time.Time {
	f.t.Helper()
	var expire time.Time
	if err := f.db.Pool.QueryRow(ctx(), `SELECT expire_time FROM credit_grants WHERE paddle_transaction_id = $1`,
		transactionID).Scan(&expire); err != nil {
		f.t.Fatal(err)
	}
	return expire
}

// refundWhileGrantExpires sends an approved refund webhook that waits for the
// billing account lock, moves the clock to the given time while it waits and
// then releases the lock. It returns the status of the webhook.
func (f *fixture) refundWhileGrantExpires(acct, txn, adjustmentID string, cents int64, clockAfterWait time.Time) int {
	f.t.Helper()
	release := f.holdAccountLock(acct)
	status := make(chan int, 1)
	go func() {
		status <- f.webhook(paddle.EventAdjustmentUpdated, paddletest.Adjustment(adjustmentID, "refund", "approved", txn, cents))
	}()
	f.waitForAccountLockWait()
	f.clock.Set(clockAfterWait)
	release()
	return <-status
}

// TestRefundExpiresGrantDueWhileWaitingForLocks checks that a refund that
// waits for the billing account lock expires the grants that became due
// during the wait before it removes credit. The refund removes nothing from
// the expired grant: the expiry is an EXPIRY entry before the REFUND entry,
// and the part of the refund that no usable grant covers becomes debt.
func TestRefundExpiresGrantDueWhileWaitingForLocks(t *testing.T) {
	t.Run("single grant", func(t *testing.T) {
		f := newFixture(t)
		acct := f.account()
		if code := f.webhook(paddle.EventTransactionCompleted, purchase("txn_exp", acct, LedgerManualTopUp, 2500)); code != http.StatusOK {
			t.Fatal(code)
		}
		expire := f.grantExpireTime("txn_exp")
		f.clock.Set(expire.Add(-time.Minute))

		if code := f.refundWhileGrantExpires(acct, "txn_exp", "adj_exp", 2500, expire.Add(time.Minute)); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		f.wantLedger(acct,
			wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
			wantEntry{LedgerExpiry, -25 * usd, 0},
			wantEntry{LedgerRefund, -25 * usd, -25 * usd},
		)
		if got := f.balance(acct); got != -25*usd {
			t.Fatalf("balance %d, want %d", got, -25*usd)
		}
		var remaining, refunded int64
		if err := f.db.Pool.QueryRow(ctx(), `SELECT remaining_usd_micros, refunded_usd_micros FROM credit_grants
			WHERE paddle_transaction_id = 'txn_exp'`).Scan(&remaining, &refunded); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 || refunded != 25*usd {
			t.Fatalf("grant remaining %d refunded %d, want 0 and %d", remaining, refunded, 25*usd)
		}
		// The refund is recorded at the time that the locks were acquired.
		var refundTime time.Time
		if err := f.db.Pool.QueryRow(ctx(), `SELECT create_time FROM credit_refunds WHERE paddle_adjustment_id = 'adj_exp'`).
			Scan(&refundTime); err != nil {
			t.Fatal(err)
		}
		if !refundTime.Equal(expire.Add(time.Minute)) {
			t.Fatalf("refund time %s, want %s", refundTime, expire.Add(time.Minute))
		}

		// A repeated or later notification of the adjustment applies nothing.
		if code := f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_exp", "refund", "approved", "txn_exp", 2500)); code != http.StatusOK {
			t.Fatalf("replay status %d", code)
		}
		f.wantLedger(acct,
			wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
			wantEntry{LedgerExpiry, -25 * usd, 0},
			wantEntry{LedgerRefund, -25 * usd, -25 * usd},
		)
		if f.refundCount() != 1 {
			t.Fatalf("credit_refunds rows = %d, want 1", f.refundCount())
		}
	})

	t.Run("another grant is consumed", func(t *testing.T) {
		f := newFixture(t)
		acct := f.account()
		start := f.clock.Now()
		f.webhook(paddle.EventTransactionCompleted, purchase("txn_first", acct, LedgerManualTopUp, 2500))
		f.clock.Set(start.AddDate(0, 1, 0))
		f.webhook(paddle.EventTransactionCompleted, purchase("txn_second", acct, LedgerManualTopUp, 1000))
		expire := f.grantExpireTime("txn_first")
		f.clock.Set(expire.Add(-time.Minute))

		// The second grant is still usable after the first one expires.
		if code := f.refundWhileGrantExpires(acct, "txn_first", "adj_first", 2500, expire.Add(time.Minute)); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		f.wantLedger(acct,
			wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
			wantEntry{LedgerManualTopUp, 10 * usd, 35 * usd},
			wantEntry{LedgerExpiry, -25 * usd, 10 * usd},
			wantEntry{LedgerRefund, -25 * usd, -15 * usd},
		)
		var remaining int64
		if err := f.db.Pool.QueryRow(ctx(), `SELECT remaining_usd_micros FROM credit_grants
			WHERE paddle_transaction_id = 'txn_second'`).Scan(&remaining); err != nil {
			t.Fatal(err)
		}
		if remaining != 0 {
			t.Fatalf("second grant remaining %d, want 0", remaining)
		}
	})

	t.Run("grant that is still usable pays first", func(t *testing.T) {
		f := newFixture(t)
		acct := f.account()
		start := f.clock.Now()
		f.webhook(paddle.EventTransactionCompleted, purchase("txn_first", acct, LedgerManualTopUp, 2500))
		f.clock.Set(start.AddDate(0, 1, 0))
		f.webhook(paddle.EventTransactionCompleted, purchase("txn_second", acct, LedgerManualTopUp, 1000))
		expire := f.grantExpireTime("txn_first")
		f.clock.Set(expire.Add(-2 * time.Minute))

		// The clock moves, but the grant of the purchase does not expire.
		if code := f.refundWhileGrantExpires(acct, "txn_first", "adj_first", 2500, expire.Add(-time.Minute)); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
		f.wantLedger(acct,
			wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
			wantEntry{LedgerManualTopUp, 10 * usd, 35 * usd},
			wantEntry{LedgerRefund, -25 * usd, 10 * usd},
		)
		if got := f.remaining(f.grantIDOf("txn_first")); got != 0 {
			t.Fatalf("refunded grant remaining %d, want 0", got)
		}
		if got := f.remaining(f.grantIDOf("txn_second")); got != 10*usd {
			t.Fatalf("other grant remaining %d, want %d", got, 10*usd)
		}
	})
}

// grantIDOf returns the grant ID of a purchase.
func (f *fixture) grantIDOf(transactionID string) string {
	f.t.Helper()
	var id string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT credit_grant_id::text FROM credit_grants WHERE paddle_transaction_id = $1`,
		transactionID).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}
