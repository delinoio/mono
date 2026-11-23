// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"errors"
	"net/http"
	"testing"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
)

// startCheckout runs StartSubscription in its own transaction, as the RPC
// does, and returns the transaction ID or the error that rolled it back.
func (f *fixture) startCheckout(acct string) (string, error) {
	f.t.Helper()
	var txnID string
	err := f.db.InTx(ctx(), func(tx pgx.Tx) error {
		var err error
		txnID, _, _, err = f.s.StartSubscription(ctx(), tx, acct)
		return err
	})
	return txnID, err
}

func (f *fixture) checkoutRows(acct string) int {
	f.t.Helper()
	return f.count(`SELECT count(*) FROM paddle_checkouts WHERE billing_account_id = $1`, acct)
}

func (f *fixture) createTransactionRequests() int {
	n := 0
	for _, r := range f.paddle.Requests() {
		if r.Method == http.MethodPost && r.Path == "/transactions" {
			n++
		}
	}
	return n
}

// A success answer whose transaction cannot open a checkout leaves no
// checkout row and no subscription change, and the caller gets the
// provider-unavailable answer. The next start call asks Paddle again.
func TestStartSubscriptionRejectsAnUnusableTransaction(t *testing.T) {
	for name, body := range map[string]string{
		"empty object": `{"data":{}}`,
		"empty ID":     `{"data":{"id":""}}`,
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			f.paddle.Respond("POST /transactions", http.StatusCreated, body)

			_, err := f.startCheckout(acct)
			var connectErr *connect.Error
			if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
				t.Fatalf("StartSubscription error = %v, want unavailable", err)
			}
			if n := f.createTransactionRequests(); n != 1 {
				t.Fatalf("create requests = %d, want 1", n)
			}
			if n := f.checkoutRows(acct); n != 0 {
				t.Fatalf("checkout rows = %d, want 0", n)
			}
			sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if err != nil || sub.Status != SubscriptionNone || sub.exists {
				t.Fatalf("subscription = %+v, %v; want none and unsaved", sub, err)
			}

			// Once Paddle answers with a transaction, the retry succeeds.
			f.paddle.Respond("POST /transactions", 0, "")
			txnID, err := f.startCheckout(acct)
			if err != nil || txnID == "" {
				t.Fatalf("retry = %q, %v", txnID, err)
			}
			if n := f.checkoutRows(acct); n != 1 {
				t.Fatalf("checkout rows after the retry = %d, want 1", n)
			}
		})
	}
}

// A valid transaction is stored and returned again while the subscription is
// pending, without another Paddle call.
func TestStartSubscriptionReusesThePendingCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()

	first, err := f.startCheckout(acct)
	if err != nil || first == "" {
		t.Fatalf("first start = %q, %v", first, err)
	}
	second, err := f.startCheckout(acct)
	if err != nil || second != first {
		t.Fatalf("second start = %q, %v; want %q", second, err, first)
	}
	if n := f.createTransactionRequests(); n != 1 {
		t.Fatalf("create requests = %d, want 1", n)
	}
	if n := f.checkoutRows(acct); n != 1 {
		t.Fatalf("checkout rows = %d, want 1", n)
	}
}

// A null entity in the success answer is not a transaction: the start call
// fails with the provider-unavailable answer and stores nothing.
func TestStartSubscriptionRejectsANullTransaction(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.paddle.Respond("POST /transactions", http.StatusCreated, `{"data":null}`)

	_, err := f.startCheckout(acct)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("StartSubscription error = %v, want unavailable", err)
	}
	if n := f.createTransactionRequests(); n != 1 {
		t.Fatalf("create requests = %d, want 1", n)
	}
	if n := f.checkoutRows(acct); n != 0 {
		t.Fatalf("checkout rows = %d, want 0", n)
	}
	if sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false); err != nil || sub.exists {
		t.Fatalf("subscription = %+v, %v; want unsaved", sub, err)
	}
}
