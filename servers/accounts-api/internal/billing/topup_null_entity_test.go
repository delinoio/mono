// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func wantChargeAcceptance(t *testing.T, f *fixture, acct string, want bool) {
	t.Helper()
	var accepted *time.Time
	if err := f.db.Pool.QueryRow(ctx(), `SELECT provider_accept_time FROM top_ups WHERE billing_account_id = $1`, acct).Scan(&accepted); err != nil {
		t.Fatal(err)
	}
	if (accepted != nil) != want {
		t.Fatalf("provider_accept_time = %v, want acceptance recorded: %v", accepted, want)
	}
}

func TestManualTopUpRecordsUnknownForUnconfirmedChargeResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"null entity", `{"data":null}`},
		{"omitted ID", `{"data":{}}`},
		{"different ID", `{"data":{"id":"sub_other"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			f.activate(acct)
			f.paddle.Respond("POST /subscriptions", http.StatusOK, tc.body)
			key, err := uuidv7.NewString()
			if err != nil {
				t.Fatal(err)
			}

			wantConnectCode(t, "first CreateTopUp", f.manualTopUp(acct, key), connect.CodeUnavailable)
			wantStatuses(t, "after the unconfirmed response", f.topUpStatuses(acct), "unknown")
			wantChargeAcceptance(t, f, acct, false)
			wantConnectCode(t, "retry before timeout", f.manualTopUp(acct, key), connect.CodeUnavailable)

			f.timeOutTopUps()
			wantStatuses(t, "after timeout", f.topUpStatuses(acct), "failed:timed_out")
			wantChargeAcceptance(t, f, acct, false)
			wantConnectCode(t, "retry after timeout", f.manualTopUp(acct, key), connect.CodeUnavailable)
			f.wantCredit(acct, 0, 0)

			charges := f.paddle.Charges()
			if len(charges) != 1 {
				t.Fatalf("charges = %d, want 1", len(charges))
			}
			now := f.clock.Now()
			body := paddletest.Event("evt_late_top_up", paddle.EventTransactionCompleted, now,
				paddletest.Transaction(paddletest.TransactionData{
					ID: f.paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge,
					TopUpID: paddletest.ChargeTopUpID(charges[0]), SubtotalCents: 2500,
				}))
			signature := paddle.SignatureHeaderValue(whSecret, now, body)
			for range 2 {
				if code := f.deliver(body, signature); code != http.StatusOK {
					t.Fatalf("webhook status = %d, want 200", code)
				}
			}
			wantStatuses(t, "after the late webhook", f.topUpStatuses(acct), "completed")
			f.wantCredit(acct, 1, 1)
			if got := f.balance(acct); got != 25*usd {
				t.Fatalf("balance = %d, want one $25 credit", got)
			}
		})
	}
}

func TestManualTopUpRecordsAcceptanceForMatchingSubscription(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	if err := f.manualTopUp(acct, topUpKey); err != nil {
		t.Fatalf("CreateTopUp: %v", err)
	}
	wantStatuses(t, "after the confirmed response", f.topUpStatuses(acct), "charged")
	wantChargeAcceptance(t, f, acct, true)
	f.wantCredit(acct, 0, 0)
}

func TestAutoTopUpKeepsUnknownForMismatchedSubscription(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.grant(acct, LedgerManualTopUp, 30*usd)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("a deduction below the threshold queued no top-up")
	}
	f.paddle.Respond("POST /subscriptions", http.StatusOK, `{"data":{"id":"sub_other"}}`)

	for range 2 {
		if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
			t.Fatalf("ProcessAutoTopUps: %v", err)
		}
		wantStatuses(t, "after the worker run", f.topUpStatuses(acct), "unknown")
		wantChargeAcceptance(t, f, acct, false)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
}

func TestTopUpOutcomeKeepsFinalizedTopUpForUnconfirmedChargeResponse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, body string }{
		{"null entity", `{"data":null}`},
		{"different ID", `{"data":{"id":"sub_other"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, acct, _ := outcomeFixture(t, outcomeTestLockHold, outcomeTestOutcomeBudget, string(TopUpCompleted))
			f.paddle.FailCharges(nil)
			f.paddle.Respond("POST /subscriptions", http.StatusOK, tc.body)

			wantConnectCode(t, "CreateTopUp", f.manualTopUp(acct, topUpKey), connect.CodeUnavailable)
			wantStatuses(t, "after the finalization", f.topUpStatuses(acct), "completed")
			wantChargeAcceptance(t, f, acct, false)
		})
	}
}
