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

func TestStartSubscriptionReusesThePendingCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()

	first, err := f.startCheckout(acct)
	if err != nil || first == "" {
		t.Fatalf("first start = %q, %v", first, err)
	}
	for _, status := range []string{paddle.TransactionStatusDraft, paddle.TransactionStatusReady, paddle.TransactionStatusBilled} {
		t.Run(status, func(t *testing.T) {
			f.paddle.SetTransactionStatus(first, status)
			id, err := f.startCheckout(acct)
			if err != nil || id != first {
				t.Fatalf("repeat start = %q, %v, want %q", id, err, first)
			}
		})
	}
	if n := f.createTransactionRequests(); n != 1 {
		t.Fatalf("create requests = %d, want 1", n)
	}
	if n := f.checkoutRows(acct); n != 1 {
		t.Fatalf("checkout rows = %d, want 1", n)
	}
}

func TestStartSubscriptionReplacesMissingCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	first, err := f.startCheckout(acct)
	if err != nil || first == "" {
		t.Fatalf("first start = %q, %v", first, err)
	}
	f.paddle.Fail("GET /transactions/"+first, &paddletest.Failure{Status: http.StatusNotFound, Code: paddle.CodeNotFound})

	replacement, err := f.startCheckout(acct)
	if err != nil || replacement == "" || replacement == first {
		t.Fatalf("replacement start = %q, %v, want a new checkout", replacement, err)
	}
	sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if err != nil || sub.Status != SubscriptionPending || sub.CheckoutTransactionID != replacement {
		t.Fatalf("subscription = %+v, %v, want pending checkout %q", sub, err, replacement)
	}
	if n := f.createTransactionRequests(); n != 2 {
		t.Fatalf("create requests = %d, want 2", n)
	}
	if n := f.checkoutRows(acct); n != 2 {
		t.Fatalf("checkout rows = %d, want 2", n)
	}
}

func TestStartSubscriptionPreservesUnconfirmedCheckout(t *testing.T) {
	for _, tt := range []struct {
		name   string
		body   string
		status int
	}{
		{"provider unavailable", `{"error":{"code":"unavailable"}}`, http.StatusServiceUnavailable},
		{"wrong error code on 404", `{"error":{"code":"route_not_found"}}`, http.StatusNotFound},
		{"empty 404", "Not Found", http.StatusNotFound},
		{"malformed response", "{", http.StatusOK},
		{"missing ID", `{"data":{"status":"canceled"}}`, http.StatusOK},
		{"mismatched canceled ID", `{"data":{"id":"txn_other","status":"canceled"}}`, http.StatusOK},
		{"missing status", `{"data":{"id":"{id}"}}`, http.StatusOK},
		{"unknown status", `{"data":{"id":"{id}","status":"unknown","collection_mode":"automatic"}}`, http.StatusOK},
		{"missing collection mode", `{"data":{"id":"{id}","status":"draft"}}`, http.StatusOK},
		{"manual collection", `{"data":{"id":"{id}","status":"draft","collection_mode":"manual"}}`, http.StatusOK},
		{"paid", `{"data":{"id":"{id}","status":"paid","collection_mode":"automatic"}}`, http.StatusOK},
		{"completed", `{"data":{"id":"{id}","status":"completed","collection_mode":"automatic"}}`, http.StatusOK},
		{"past due transaction", `{"data":{"id":"{id}","status":"past_due","collection_mode":"automatic"}}`, http.StatusOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			first, err := f.startCheckout(acct)
			if err != nil || first == "" {
				t.Fatalf("first start = %q, %v", first, err)
			}
			f.paddle.Respond("GET /transactions/"+first, tt.status, strings.ReplaceAll(tt.body, "{id}", first))

			id, err := f.startCheckout(acct)
			if id != "" || connect.CodeOf(err) != connect.CodeUnavailable {
				t.Fatalf("repeat start = %q, %v, want unavailable", id, err)
			}
			sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if err != nil || sub.Status != SubscriptionPending || sub.CheckoutTransactionID != first {
				t.Fatalf("subscription = %+v, %v, want unchanged pending checkout %q", sub, err, first)
			}
			if n := f.createTransactionRequests(); n != 1 {
				t.Fatalf("create requests = %d, want 1", n)
			}
			if n := f.checkoutRows(acct); n != 1 {
				t.Fatalf("checkout rows = %d, want 1", n)
			}
		})
	}
}

func TestStartSubscriptionRejectsExistingSubscription(t *testing.T) {
	for _, status := range []SubscriptionStatus{SubscriptionActive, SubscriptionPastDue} {
		t.Run(string(status), func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			f.tx(func(tx pgx.Tx) error {
				return saveSubscription(ctx(), tx, acct, Subscription{Status: status}, "", f.s.timestamp())
			})

			if id, err := f.startCheckout(acct); id != "" || connect.CodeOf(err) != connect.CodeAlreadyExists {
				t.Fatalf("start = %q, %v, want already_exists", id, err)
			}
			if calls := f.paddle.Requests(); len(calls) != 0 {
				t.Fatalf("Paddle calls = %v, want none", calls)
			}
		})
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
