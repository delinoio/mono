// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestPaymentWebhookExpiresCreditBeforePurchase(t *testing.T) {
	for _, tc := range []struct {
		name          string
		kind          LedgerKind
		releaseOffset time.Duration
	}{
		{name: "manual top-up at expiry", kind: LedgerManualTopUp},
		{name: "subscription payment at expiry", kind: LedgerSubscriptionCredit},
		{name: "purchase before expiry", kind: LedgerManualTopUp, releaseOffset: -time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			const seedID, txnID, eventID = "txn_seed", "txn_purchase", "evt_purchase"
			if code := f.webhook(paddle.EventTransactionCompleted, purchase(seedID, acct, LedgerManualTopUp, 400)); code != http.StatusOK {
				t.Fatalf("seed payment status %d", code)
			}
			if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO credit_settings (billing_account_id,
				low_balance_threshold_usd_micros, low_balance_alerted, update_time) VALUES ($1, $2, true, $3)`,
				acct, 12*usd, f.clock.Now()); err != nil {
				t.Fatal(err)
			}

			accountingTime := f.grantExpireTime(seedID).Add(tc.releaseOffset)
			eventTime := accountingTime.Add(-time.Minute)
			f.clock.Set(eventTime)
			data := purchase(txnID, acct, tc.kind, 1000)
			server := httptest.NewServer(f.s.WebhookHandler())
			t.Cleanup(server.Close)
			client := server.Client()
			client.Timeout = 30 * time.Second
			deliver := func(id string, signatureTime time.Time) error {
				body := paddletest.Event(id, paddle.EventTransactionCompleted, eventTime, data)
				req, err := http.NewRequest(http.MethodPost, server.URL+"/webhooks/paddle", bytes.NewReader(body))
				if err != nil {
					return err
				}
				req.Header.Set(paddle.SignatureHeader, paddle.SignatureHeaderValue(whSecret, signatureTime, body))
				resp, err := client.Do(req)
				if err != nil {
					return err
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					return fmt.Errorf("payment %s status %d, want %d", id, resp.StatusCode, http.StatusOK)
				}
				return nil
			}

			release := f.holdAccountLock(acct)
			result := make(chan error, 1)
			go func() { result <- deliver(eventID, eventTime) }()
			f.waitForAccountLockWait()
			f.clock.Set(accountingTime)
			release()
			if err := <-result; err != nil {
				t.Fatal(err)
			}

			wantExpired := tc.releaseOffset == 0
			wantSeedRemaining := 4 * usd
			want := []wantEntry{{LedgerManualTopUp, 4 * usd, 4 * usd}}
			if wantExpired {
				want = append(want, wantEntry{LedgerExpiry, -4 * usd, 0})
				wantSeedRemaining = 0
			}
			want = append(want, wantEntry{tc.kind, 10 * usd, 10*usd + wantSeedRemaining})
			f.wantLedger(acct, want...)
			f.wantLowBalance(acct, 0, wantExpired)
			if got := f.remaining(f.grantIDOf(seedID)); got != wantSeedRemaining {
				t.Errorf("seed grant remaining %d, want %d", got, wantSeedRemaining)
			}
			entries, err := ListLedgerByTime(ctx(), f.db.Pool, acct, time.Time{}, "", len(want))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != len(want) || entries[0].Kind != tc.kind || deref(entries[0].PaddleTransactionID) != txnID ||
				!entries[0].CreateTime.Equal(accountingTime) {
				t.Fatalf("newest ledger entry must be the purchase at %s, got %+v", accountingTime, entries)
			}
			if wantExpired && (entries[1].Kind != LedgerExpiry || !entries[1].CreateTime.Equal(accountingTime)) {
				t.Fatalf("second newest ledger entry must be the expiry at %s, got %+v", accountingTime, entries[1])
			}

			var createdAt, expiresAt, processedAt time.Time
			var remaining int64
			if err := f.db.Pool.QueryRow(ctx(), `SELECT create_time, expire_time, remaining_usd_micros
				FROM credit_grants WHERE paddle_transaction_id = $1`, txnID).Scan(&createdAt, &expiresAt, &remaining); err != nil {
				t.Fatal(err)
			}
			if err := f.db.Pool.QueryRow(ctx(), `SELECT process_time FROM paddle_events WHERE paddle_event_id = $1`, eventID).
				Scan(&processedAt); err != nil {
				t.Fatal(err)
			}
			if !createdAt.Equal(eventTime) || !expiresAt.Equal(GrantExpireTime(eventTime)) ||
				!processedAt.Equal(eventTime) || remaining != 10*usd {
				t.Errorf("grant created at %s, expires at %s, has %d remaining and event processed at %s, want %s, %s, %d and %s",
					createdAt, expiresAt, remaining, processedAt, eventTime, GrantExpireTime(eventTime), 10*usd, eventTime)
			}

			for _, id := range []string{eventID, "evt_purchase_redelivery"} {
				if err := deliver(id, accountingTime); err != nil {
					t.Fatal(err)
				}
			}
			f.wantLedger(acct, want...)
			f.wantLowBalance(acct, 0, wantExpired)
			if got := f.count(`SELECT count(*) FROM credit_grants WHERE paddle_transaction_id = $1`, txnID); got != 1 {
				t.Fatalf("after replays: grants %d, want 1", got)
			}
			t.Logf("observed account lock wait, purchase balance %d, prior grant remaining %d, alert flagged %v, no duplicate credit",
				entries[0].BalanceAfter, f.remaining(f.grantIDOf(seedID)), wantExpired)
		})
	}
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
