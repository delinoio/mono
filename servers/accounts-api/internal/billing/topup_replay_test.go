// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// wantConnectCode checks that err is a Connect error with the code.
func wantConnectCode(t *testing.T, label string, err error, want connect.Code) {
	t.Helper()
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != want {
		t.Fatalf("%s: error = %v, want code %v", label, err, want)
	}
}

// timeOutTopUps advances the clock past TopUpTimeout and runs the
// time-out task.
func (f *fixture) timeOutTopUps() {
	f.t.Helper()
	f.clock.Advance(TopUpTimeout + time.Minute)
	if err := f.s.TimeOutTopUps(ctx()); err != nil {
		f.t.Fatal(err)
	}
}

// wantCredit checks the number of credit grants and of manual top-up ledger
// entries of the account.
func (f *fixture) wantCredit(acct string, grants, entries int) {
	f.t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM credit_grants WHERE billing_account_id = $1`, acct).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	if n != grants {
		f.t.Fatalf("credit grants = %d, want %d", n, grants)
	}
	n = 0
	for _, e := range f.ledger(acct) {
		if e.Kind == LedgerManualTopUp {
			n++
		}
	}
	if n != entries {
		f.t.Fatalf("manual top-up ledger entries = %d, want %d", n, entries)
	}
}

func TestManualTopUpReplayAfterAcceptedChargeTimedOutSucceeds(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)

	if err := f.manualTopUp(acct, topUpKey); err != nil {
		t.Fatalf("first CreateTopUp: %v", err)
	}
	wantStatuses(t, "after the charge", f.topUpStatuses(acct), "charged")
	f.timeOutTopUps()
	wantStatuses(t, "after the timeout", f.topUpStatuses(acct), "failed:timed_out")

	// The replay within the idempotency window reports the accepted charge
	// and sends no second one; another amount is still rejected.
	if err := f.manualTopUp(acct, topUpKey); err != nil {
		t.Fatalf("replay after the timeout: %v", err)
	}
	err := f.s.CreateTopUp(ctx(), acct, "admin-user", topUpKey, 50*usd, func(pgx.Tx) error { return nil }, nil, nil)
	wantConnectCode(t, "replay with another amount", err, connect.CodeInvalidArgument)
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}

	// The late verified webhook still adds exactly one grant and one ledger
	// entry; a duplicate delivery adds neither again.
	charge := f.paddle.Charges()[0]
	body := paddletest.Event("evt_late_top_up", paddle.EventTransactionCompleted, f.clock.Now(),
		paddletest.Transaction(paddletest.TransactionData{
			ID: f.paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge, TopUpID: paddletest.ChargeTopUpID(charge),
			SubtotalCents: 2500, TaxCents: 250,
		}))
	signature := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	for range 2 {
		if code := f.deliver(body, signature); code != http.StatusOK {
			t.Fatalf("webhook status %d", code)
		}
	}
	wantStatuses(t, "after the late webhook", f.topUpStatuses(acct), "completed")
	f.wantCredit(acct, 1, 1)
	if got := f.balance(acct); got != 25*usd {
		t.Fatalf("balance = %d, want one credit", got)
	}
	if err := f.manualTopUp(acct, topUpKey); err != nil {
		t.Fatalf("replay after the completion: %v", err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges after the replays = %d, want 1", n)
	}
}

func TestManualTopUpReplayAfterUnknownResultStaysUnavailable(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadGateway, Code: "bad_gateway"})

	wantConnectCode(t, "first CreateTopUp", f.manualTopUp(acct, topUpKey), connect.CodeUnavailable)
	wantStatuses(t, "after the unknown result", f.topUpStatuses(acct), "unknown")
	wantConnectCode(t, "replay before the timeout", f.manualTopUp(acct, topUpKey), connect.CodeUnavailable)

	// The timeout has no evidence that Paddle accepted the charge, so the
	// replay is not a success.
	f.timeOutTopUps()
	wantStatuses(t, "after the timeout", f.topUpStatuses(acct), "failed:timed_out")
	wantConnectCode(t, "replay after the timeout", f.manualTopUp(acct, topUpKey), connect.CodeUnavailable)
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
}

// A row that timed out before the provider_accept_time column existed has no
// evidence of acceptance.
func TestManualTopUpReplayOfLegacyTimedOutRowIsUnavailable(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	sub := f.activate(acct)
	now := f.clock.Now()
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind, idempotency_key, actor_user_id,
			amount_usd_micros, status, failure_reason, paddle_subscription_id, create_time, update_time)
		VALUES ('01900000-0000-7000-8000-0000000000d1', $1, 'manual', $2, 'admin-user', $3, 'failed', 'timed_out', $4, $5, $5)`,
		acct, topUpKey, 25*usd, sub, now); err != nil {
		t.Fatal(err)
	}
	wantConnectCode(t, "replay of the legacy row", f.manualTopUp(acct, topUpKey), connect.CodeUnavailable)
	if n := len(f.paddle.Charges()); n != 0 {
		t.Fatalf("charges = %d, want none", n)
	}
}

func TestManualTopUpReplayAfterRejectionIsFailedPrecondition(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})

	wantConnectCode(t, "first CreateTopUp", f.manualTopUp(acct, topUpKey), connect.CodeFailedPrecondition)
	wantStatuses(t, "after the rejection", f.topUpStatuses(acct), "failed:payment_declined")
	wantConnectCode(t, "replay", f.manualTopUp(acct, topUpKey), connect.CodeFailedPrecondition)
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
}

// A payment that fails after Paddle accepted the charge is a recorded
// failure, not a timeout: the replay keeps the failed_precondition answer.
func TestManualTopUpReplayAfterFailedPaymentIsFailedPrecondition(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	if err := f.manualTopUp(acct, topUpKey); err != nil {
		t.Fatal(err)
	}
	charge := f.paddle.Charges()[0]
	if code := f.webhook(paddle.EventTransactionPaymentFailed, paddletest.Transaction(paddletest.TransactionData{
		ID: f.paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge, TopUpID: paddletest.ChargeTopUpID(charge),
		SubtotalCents: 2500, TaxCents: 250,
	})); code != http.StatusOK {
		t.Fatalf("webhook status %d", code)
	}
	wantStatuses(t, "after the failed payment", f.topUpStatuses(acct), "failed:payment_failed")
	wantConnectCode(t, "replay", f.manualTopUp(acct, topUpKey), connect.CodeFailedPrecondition)
}
