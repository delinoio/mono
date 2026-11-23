// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// closeAccount runs CloseAccount in its own transaction, as
// DeleteBillingAccount does, and returns the error that rolled it back.
func (f *fixture) closeAccount(acct string) error {
	f.t.Helper()
	return f.db.InTx(ctx(), func(tx pgx.Tx) error { return f.s.CloseAccount(ctx(), tx, acct) })
}

// wantPendingCheckout asserts that the billing account still has the pending
// subscription with the open checkout transaction and its checkout record.
func (f *fixture) wantPendingCheckout(acct, txnID string) {
	f.t.Helper()
	stored, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if err != nil {
		f.t.Fatal(err)
	}
	if stored.Status != SubscriptionPending || stored.CheckoutTransactionID != txnID {
		f.t.Fatalf("subscription = %s with checkout %q, want PENDING with %q", stored.Status, stored.CheckoutTransactionID, txnID)
	}
	if n := f.count(`SELECT count(*) FROM paddle_checkouts WHERE billing_account_id = $1 AND paddle_transaction_id = $2`, acct, txnID); n != 1 {
		f.t.Fatalf("checkout records = %d, want 1", n)
	}
}

func (f *fixture) patchTransactionRequests() int {
	n := 0
	for _, r := range f.paddle.Requests() {
		if r.Method == http.MethodPatch && strings.HasPrefix(r.Path, "/transactions/") {
			n++
		}
	}
	return n
}

// An account whose first checkout is still open has no subscription to
// cancel, so the deletion must cancel the checkout transaction: Paddle would
// otherwise collect a payment for it after the deletion.
func TestCloseAccountCancelsPendingCheckoutTransaction(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	txnID := f.checkout(acct)
	f.grant(acct, LedgerManualTopUp, 25*usd)

	if err := f.closeAccount(acct); err != nil {
		t.Fatal(err)
	}
	txn, ok := f.paddle.Transaction(txnID)
	if !ok || txn.Status != paddle.TransactionStatusCanceled {
		t.Fatalf("Paddle checkout transaction = %+v, want canceled", txn)
	}
	stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if stored.Status != SubscriptionCanceled || stored.CheckoutTransactionID != "" {
		t.Fatalf("stored subscription = %s with checkout %q", stored.Status, stored.CheckoutTransactionID)
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
	)
}

// A provider error leaves the checkout record, the pending subscription, the
// open Paddle transaction and the credit as they were, and the caller can
// retry.
func TestCloseAccountPendingCheckoutProviderFailureChangesNothing(t *testing.T) {
	for name, tc := range map[string]struct {
		failure *paddletest.Failure
		raw     string
		want    connect.Code
	}{
		"unavailable":     {failure: &paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"}, want: connect.CodeUnavailable},
		"timeout":         {failure: &paddletest.Failure{Status: http.StatusBadGateway, Code: "bad_gateway", Truncate: true}, want: connect.CodeUnavailable},
		"rate limited":    {failure: &paddletest.Failure{Status: http.StatusTooManyRequests, Code: "too_many_requests"}, want: connect.CodeUnavailable},
		"rejected":        {failure: &paddletest.Failure{Status: http.StatusBadRequest, Code: "transaction_immutable"}, want: connect.CodeFailedPrecondition},
		"empty object":    {raw: `{"data":{}}`, want: connect.CodeUnavailable},
		"null entity":     {raw: `{"data":null}`, want: connect.CodeUnavailable},
		"other ID":        {raw: `{"data":{"id":"txn_other","status":"canceled"}}`, want: connect.CodeUnavailable},
		"not canceled":    {raw: `{"data":{"id":"%ID%","status":"billed"}}`, want: connect.CodeUnavailable},
		"malformed 2xx":   {raw: `not json`, want: connect.CodeUnavailable},
		"missing status":  {raw: `{"data":{"id":"%ID%"}}`, want: connect.CodeUnavailable},
		"unknown success": {raw: `{"data":{"id":"%ID%","status":"completed"}}`, want: connect.CodeUnavailable},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			txnID := f.checkout(acct)
			f.grant(acct, LedgerManualTopUp, 25*usd)
			if tc.failure != nil {
				f.paddle.Fail("PATCH /transactions/", tc.failure)
			} else {
				body := strings.ReplaceAll(tc.raw, "%ID%", txnID)
				f.paddle.Respond("PATCH /transactions/", http.StatusOK, body)
			}

			wantConnectCode(t, "close account", f.closeAccount(acct), tc.want)
			f.wantPendingCheckout(acct, txnID)
			if f.balance(acct) != 25*usd {
				t.Fatalf("balance = %d, want the credit untouched", f.balance(acct))
			}
			f.wantLedger(acct, wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd})
			if txn, _ := f.paddle.Transaction(txnID); txn.Status != paddle.TransactionStatusDraft {
				t.Fatalf("Paddle checkout transaction status = %s, want draft", txn.Status)
			}

			// The failure is gone: the retry closes the checkout.
			f.paddle.Fail("PATCH /transactions/", nil)
			f.paddle.Respond("PATCH /transactions/", 0, "")
			if err := f.closeAccount(acct); err != nil {
				t.Fatal(err)
			}
			if txn, _ := f.paddle.Transaction(txnID); txn.Status != paddle.TransactionStatusCanceled {
				t.Fatalf("Paddle checkout transaction status = %s, want canceled", txn.Status)
			}
		})
	}
}

// A payment that completed the checkout before the cancellation cannot be
// canceled any more. The deletion fails with failed_precondition and keeps
// everything; after the subscription webhook names the subscription, the
// retry cancels the subscription as for any established subscription.
func TestCloseAccountPendingCheckoutPaidMeanwhile(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	txnID := f.checkout(acct)
	f.paddle.SetTransactionStatus(txnID, paddle.TransactionStatusCompleted)

	wantConnectCode(t, "close account", f.closeAccount(acct), connect.CodeFailedPrecondition)
	f.wantPendingCheckout(acct, txnID)

	subID := f.paddle.NewID("sub")
	f.paddle.SetSubscription(paddle.Subscription{ID: subID, Status: paddle.SubscriptionStatusActive, TransactionID: txnID})
	f.tx(func(tx pgx.Tx) error {
		acctState, err := lockAccount(ctx(), tx, acct)
		if err != nil {
			return err
		}
		sub, _ := f.paddle.Subscription(subID)
		sub.CustomerID = "ctm_1"
		_, err = f.s.applyPaddleSubscription(ctx(), tx, acctState, sub, f.s.timestamp())
		return err
	})
	if err := f.closeAccount(acct); err != nil {
		t.Fatal(err)
	}
	if sub, _ := f.paddle.Subscription(subID); sub.Status != paddle.SubscriptionStatusCanceled {
		t.Fatalf("Paddle subscription %s, want canceled", sub.Status)
	}
	stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if stored.Status != SubscriptionCanceled {
		t.Fatalf("stored status %s", stored.Status)
	}
}

// A checkout transaction that Paddle does not know any more, or that Paddle
// already canceled, counts as closed.
func TestCloseAccountPendingCheckoutAlreadyClosedAtPaddle(t *testing.T) {
	t.Run("not found", func(t *testing.T) {
		f := newFixture(t)
		acct := f.account()
		f.checkout(acct)
		f.paddle.Fail("PATCH /transactions/", &paddletest.Failure{Status: http.StatusNotFound, Code: "not_found"})
		if err := f.closeAccount(acct); err != nil {
			t.Fatal(err)
		}
		stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
		if stored.Status != SubscriptionCanceled || stored.CheckoutTransactionID != "" {
			t.Fatalf("stored subscription = %s with checkout %q", stored.Status, stored.CheckoutTransactionID)
		}
	})
	t.Run("already canceled", func(t *testing.T) {
		f := newFixture(t)
		acct := f.account()
		txnID := f.checkout(acct)
		f.paddle.SetTransactionStatus(txnID, paddle.TransactionStatusCanceled)
		if err := f.closeAccount(acct); err != nil {
			t.Fatal(err)
		}
		stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
		if stored.Status != SubscriptionCanceled || stored.CheckoutTransactionID != "" {
			t.Fatalf("stored subscription = %s with checkout %q", stored.Status, stored.CheckoutTransactionID)
		}
	})
}

// An account that never started a checkout, and an established
// subscription, make no checkout transaction call.
func TestCloseAccountMakesNoCheckoutCallWithoutOpenCheckout(t *testing.T) {
	f := newFixture(t)
	empty := f.account()
	if err := f.closeAccount(empty); err != nil {
		t.Fatal(err)
	}
	active := f.account()
	f.activate(active)
	if err := f.closeAccount(active); err != nil {
		t.Fatal(err)
	}
	if n := f.patchTransactionRequests(); n != 0 {
		t.Fatalf("%d transaction cancel requests, want 0", n)
	}
}

func (f *fixture) cancelRequests() (immediate int) {
	for _, r := range f.paddle.Requests() {
		if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/cancel") && r.Body["effective_from"] == string(paddle.EffectiveImmediately) {
			immediate++
		}
	}
	return immediate
}

func (f *fixture) getSubscriptionRequests() int {
	n := 0
	for _, r := range f.paddle.Requests() {
		if r.Method == http.MethodGet && strings.HasPrefix(r.Path, "/subscriptions/") {
			n++
		}
	}
	return n
}

var errLaterWrite = errors.New("a later write of the deletion failed")

// Paddle confirms the cancellation, then a later write of the deletion
// transaction fails and the local rows roll back. The retry has to complete
// the local deletion from what Paddle reports: the subscription is canceled,
// so the retry must not need a second cancellation, and the credit is
// forfeited once.
func TestCloseAccountRetryAfterRollbackReconcilesWithPaddle(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	f.grant(acct, LedgerManualTopUp, 25*usd)

	err := f.db.InTx(ctx(), func(tx pgx.Tx) error {
		if err := f.s.CloseAccount(ctx(), tx, acct); err != nil {
			return err
		}
		return errLaterWrite
	})
	if !errors.Is(err, errLaterWrite) {
		t.Fatalf("first attempt = %v", err)
	}
	if sub, _ := f.paddle.Subscription(subID); sub.Status != paddle.SubscriptionStatusCanceled {
		t.Fatalf("Paddle subscription %s, want canceled by the first attempt", sub.Status)
	}
	// The rollback restored the local rows, and the credit is intact.
	stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if stored.Status != SubscriptionActive || stored.PaddleSubscriptionID != subID {
		t.Fatalf("stored subscription after the rollback = %s %q", stored.Status, stored.PaddleSubscriptionID)
	}
	f.wantLedger(acct, wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd})

	if err := f.closeAccount(acct); err != nil {
		t.Fatalf("retry = %v", err)
	}
	stored, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if stored.Status != SubscriptionCanceled {
		t.Fatalf("stored status after the retry = %s", stored.Status)
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
	)
	if f.getSubscriptionRequests() != 1 {
		t.Fatalf("the retry made %d subscription reads, want 1", f.getSubscriptionRequests())
	}

	// A further attempt on the committed deletion finds the local
	// subscription canceled and makes no Paddle call and no second forfeiture.
	before := len(f.paddle.Requests())
	if err := f.closeAccount(acct); err != nil {
		t.Fatal(err)
	}
	if len(f.paddle.Requests()) != before {
		t.Fatal("a closed account called Paddle again")
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
	)
}

// When the retry cannot read the subscription at Paddle, or Paddle rejects
// the cancellation of a subscription that is not canceled, the deletion fails
// and changes nothing.
func TestCloseAccountRetryAfterRollbackNeedsPaddleConfirmation(t *testing.T) {
	setup := func(t *testing.T) (*fixture, string, string) {
		f := newFixture(t)
		acct := f.account()
		subID := f.activate(acct)
		f.grant(acct, LedgerManualTopUp, 25*usd)
		return f, acct, subID
	}
	t.Run("read fails", func(t *testing.T) {
		f, acct, subID := setup(t)
		_ = f.db.InTx(ctx(), func(tx pgx.Tx) error {
			if err := f.s.CloseAccount(ctx(), tx, acct); err != nil {
				return err
			}
			return errLaterWrite
		})
		f.paddle.Fail("GET /subscriptions/", &paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"})
		wantConnectCode(t, "retry", f.closeAccount(acct), connect.CodeUnavailable)
		stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
		if stored.Status != SubscriptionActive || stored.PaddleSubscriptionID != subID {
			t.Fatalf("stored subscription = %s", stored.Status)
		}
		f.wantLedger(acct, wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd})
		// Paddle is readable again: the retry completes.
		f.paddle.Fail("GET /subscriptions/", nil)
		if err := f.closeAccount(acct); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("rejected while active", func(t *testing.T) {
		f, acct, subID := setup(t)
		f.paddle.Fail("POST /subscriptions/", &paddletest.Failure{Status: http.StatusBadRequest, Code: "subscription_locked_pending_changes"})
		wantConnectCode(t, "close", f.closeAccount(acct), connect.CodeFailedPrecondition)
		if sub, _ := f.paddle.Subscription(subID); sub.Status != paddle.SubscriptionStatusActive {
			t.Fatalf("Paddle subscription %s", sub.Status)
		}
		stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
		if stored.Status != SubscriptionActive {
			t.Fatalf("stored status = %s", stored.Status)
		}
		f.wantLedger(acct, wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd})
	})
}

// The immediate cancellation must be confirmed. A 2xx answer without the
// canceled subscription of the account leaves the account, the subscription
// and the credit as they were and fails with unavailable, so the owner can
// retry; a confirmed cancellation and an explicit not-found still complete the
// deletion.
func TestCloseAccountRequiresConfirmedCancellation(t *testing.T) {
	const canceled = `{"data":{"id":"%ID%","status":"canceled"}}`
	for name, tc := range map[string]struct {
		status int
		body   string
		want   connect.Code
	}{
		"null entity":             {http.StatusOK, `{"data":null}`, connect.CodeUnavailable},
		"empty entity":            {http.StatusOK, `{"data":{}}`, connect.CodeUnavailable},
		"missing data":            {http.StatusOK, `{}`, connect.CodeUnavailable},
		"not JSON":                {http.StatusOK, `ok`, connect.CodeUnavailable},
		"other subscription":      {http.StatusOK, `{"data":{"id":"sub_other","status":"canceled"}}`, connect.CodeUnavailable},
		"status active":           {http.StatusOK, `{"data":{"id":"%ID%","status":"active"}}`, connect.CodeUnavailable},
		"scheduled cancellation":  {http.StatusOK, `{"data":{"id":"%ID%","status":"active","scheduled_change":{"action":"cancel","effective_at":"2099-01-01T00:00:00Z"}}}`, connect.CodeUnavailable},
		"missing status":          {http.StatusOK, `{"data":{"id":"%ID%"}}`, connect.CodeUnavailable},
		"unknown status":          {http.StatusOK, `{"data":{"id":"%ID%","status":"archived"}}`, connect.CodeUnavailable},
		"accepted without entity": {http.StatusAccepted, `{"data":{}}`, connect.CodeUnavailable},
		"confirmed":               {http.StatusOK, canceled, 0},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			subID := f.activate(acct)
			f.grant(acct, LedgerManualTopUp, 25*usd)
			f.paddle.Respond("POST /subscriptions/", tc.status, strings.ReplaceAll(tc.body, "%ID%", subID))

			err := f.closeAccount(acct)
			stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if tc.want == 0 {
				if err != nil || stored.Status != SubscriptionCanceled {
					t.Fatalf("closeAccount = %v, stored status %s; want a completed deletion", err, stored.Status)
				}
				f.wantLedger(acct,
					wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
					wantEntry{LedgerExpiry, -25 * usd, 0},
				)
				return
			}
			wantConnectCode(t, "close account", err, tc.want)
			if stored.Status != SubscriptionActive || stored.PaddleSubscriptionID != subID {
				t.Fatalf("stored subscription = %s %q, want it unchanged", stored.Status, stored.PaddleSubscriptionID)
			}
			f.wantLedger(acct, wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd})
			if f.balance(acct) != 25*usd {
				t.Fatalf("balance = %d, want the credit intact", f.balance(acct))
			}
			// The Paddle answer is fine again: the retry completes.
			f.paddle.Respond("POST /subscriptions/", 0, "")
			if err := f.closeAccount(acct); err != nil {
				t.Fatalf("retry = %v", err)
			}
		})
	}
}

// An explicit not-found of the subscription at Paddle still completes the
// deletion.
func TestCloseAccountCompletesWhenPaddleDoesNotKnowTheSubscription(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.paddle.Fail("POST /subscriptions/", &paddletest.Failure{Status: http.StatusNotFound, Code: "not_found"})
	if err := f.closeAccount(acct); err != nil {
		t.Fatal(err)
	}
	stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if stored.Status != SubscriptionCanceled {
		t.Fatalf("stored status %s", stored.Status)
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
	)
}
