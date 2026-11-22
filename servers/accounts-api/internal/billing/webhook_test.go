// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func (f *fixture) eventCount() int {
	f.t.Helper()
	var n int
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM paddle_events`).Scan(&n); err != nil {
		f.t.Fatal(err)
	}
	return n
}

// checkout starts the subscription and returns the checkout transaction ID.
func (f *fixture) checkout(accountID string) string {
	f.t.Helper()
	var txnID string
	f.tx(func(tx pgx.Tx) error {
		var err error
		txnID, _, _, err = f.s.StartSubscription(ctx(), tx, accountID)
		return err
	})
	return txnID
}

func TestWebhookSignatureRejection(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	data := paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_1", Origin: paddle.OriginWeb, BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000,
	})
	body := paddletest.Event("evt_sig", paddle.EventTransactionCompleted, f.clock.Now(), data)
	for name, sig := range map[string]string{
		"unsigned":     "",
		"other secret": paddle.SignatureHeaderValue("wrong", f.clock.Now(), body),
		"stale":        paddle.SignatureHeaderValue(whSecret, f.clock.Now().Add(-6*time.Minute), body),
		"future":       paddle.SignatureHeaderValue(whSecret, f.clock.Now().Add(6*time.Minute), body),
		"garbage":      "ts=abc;h1=xyz",
	} {
		if code := f.deliver(body, sig); code != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", name, code)
		}
	}
	// A body changed after signing is rejected too.
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	tampered := append([]byte(nil), body...)
	tampered[len(tampered)-2] = ' '
	if code := f.deliver(tampered, sig); code != http.StatusUnauthorized {
		t.Errorf("tampered body: status %d", code)
	}
	if f.balance(acct) != 0 || f.eventCount() != 0 {
		t.Fatalf("a rejected delivery changed state: balance %d, events %d", f.balance(acct), f.eventCount())
	}
	if code := f.deliver(body, sig); code != http.StatusOK || f.balance(acct) != 10*usd {
		t.Fatalf("signed delivery: status %d, balance %d", code, f.balance(acct))
	}
}

func TestWebhookSubscriptionPaymentAndReplay(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	txnID := f.checkout(acct)
	// A repeated start returns the open checkout.
	if again := f.checkout(acct); again != txnID {
		t.Fatalf("second StartSubscription = %s, want %s", again, txnID)
	}
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionPending {
		t.Fatalf("status %s", sub.Status)
	}

	subID := f.paddle.NewID("sub")
	periodEnd := f.clock.Now().AddDate(0, 1, 0).Truncate(time.Second)
	// The payment arrives first, without custom data: the checkout record
	// finds the billing account.
	data := paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: subID, Origin: paddle.OriginWeb, PriceID: priceID, SubtotalCents: 1000, TaxCents: 100,
		PeriodEnd: periodEnd,
	})
	body := paddletest.Event("evt_pay", paddle.EventTransactionCompleted, f.clock.Now(), data)
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	for range 3 {
		if code := f.deliver(body, sig); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	}
	// The same transaction in another event adds nothing either.
	if code := f.webhook(paddle.EventTransactionCompleted, data); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != subID || sub.CurrentPeriodEnd == nil ||
		!sub.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("subscription after the payment: %+v", sub)
	}

	// Subscription events track the status; an older entity is ignored.
	entity := map[string]any{
		"id": subID, "status": "active", "transaction_id": txnID, "updated_at": f.clock.Now().Add(time.Minute),
		"current_billing_period": map[string]any{"starts_at": f.clock.Now(), "ends_at": periodEnd},
		"scheduled_change":       map[string]any{"action": "cancel", "effective_at": periodEnd},
	}
	f.webhook("subscription.updated", entity)
	stale := map[string]any{"id": subID, "status": "past_due", "updated_at": f.clock.Now()}
	f.webhook("subscription.past_due", stale)
	sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || !sub.CancelAtPeriodEnd {
		t.Fatalf("subscription after the events: %+v", sub)
	}
	// A canceled subscription ends at the period end.
	f.webhook("subscription.canceled", map[string]any{"id": subID, "status": "canceled", "updated_at": f.clock.Now().Add(time.Hour)})
	sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionCanceled || sub.CurrentPeriodEnd != nil {
		t.Fatalf("canceled subscription: %+v", sub)
	}
	// A renewal of another subscription is found through custom data and
	// still adds credit.
	f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_other", Origin: paddle.OriginSubscriptionRecurring, BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000,
	}))
	if f.balance(acct) != 20*usd {
		t.Fatalf("balance %d", f.balance(acct))
	}
}

func TestWebhookRefundAndChargeback(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_buy", Origin: paddle.OriginWeb, BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000,
	}))
	f.deduct(acct, 4*usd)
	// A refund that waits for approval changes nothing.
	f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_1", "refund", "pending_approval", "txn_buy", 1000))
	if f.balance(acct) != 6*usd {
		t.Fatalf("balance %d", f.balance(acct))
	}
	// The approval removes the credit, and the balance becomes negative.
	f.webhook(paddle.EventAdjustmentUpdated, paddletest.Adjustment("adj_1", "refund", "approved", "txn_buy", 1000))
	f.webhook(paddle.EventAdjustmentUpdated, paddletest.Adjustment("adj_1", "refund", "approved", "txn_buy", 1000))
	// Another adjustment cannot remove more than the purchase added.
	f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_2", "chargeback", "approved", "txn_buy", 1000))
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerUsage, -4 * usd, 6 * usd},
		wantEntry{LedgerRefund, -10 * usd, -4 * usd},
	)
	// An unknown transaction is unmatched and changes nothing.
	if code := f.webhook(paddle.EventAdjustmentCreated, paddletest.Adjustment("adj_3", "refund", "approved", "txn_unknown", 500)); code != http.StatusOK {
		t.Fatal(code)
	}
}

func TestWebhookFailedRenewalAndChargeFailure(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	failed := paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_renew", SubscriptionID: subID, Origin: paddle.OriginSubscriptionRecurring, PriceID: priceID,
		SubtotalCents: 1000, Status: "past_due",
	})
	f.webhook(paddle.EventTransactionPaymentFailed, failed)
	f.webhook(paddle.EventTransactionPaymentFailed, failed) // Paddle retries: one email per transaction
	// A failed attempt inside the checkout sends nothing.
	f.webhook(paddle.EventTransactionPaymentFailed, paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_checkout", Origin: paddle.OriginWeb, BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000,
	}))
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	msgs := f.email.Messages()
	if len(msgs) != 2 || msgs[0].Kind != email.KindPaymentFailed {
		t.Fatalf("emails %+v", msgs)
	}
	// Retrying the delivery sends nothing again.
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if len(f.email.Messages()) != 2 {
		t.Fatal("a notification was sent twice")
	}

	// A one-time charge whose payment fails later fails the top-up once.
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	f.deduct(acct, usd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	charge := f.paddle.Charges()[0]
	data := paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_charge", SubscriptionID: subID, Origin: paddle.OriginSubscriptionCharge,
		TopUpID: paddletest.ChargeTopUpID(charge), SubtotalCents: 2500, Status: "past_due",
	})
	f.webhook(paddle.EventTransactionPaymentFailed, data)
	f.webhook(paddle.EventTransactionPaymentFailed, data)
	st, _ := loadSettings(ctx(), f.db.Pool, acct)
	if st.ConsecutiveFailures != 1 {
		t.Fatalf("failures %d, want 1", st.ConsecutiveFailures)
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "failed:payment_failed" {
		t.Fatalf("top-ups %v", got)
	}
}

func TestEmailDeliveryRetry(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.deduct(acct, 20*usd)
	f.email.SetErr(errSendTest)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if len(f.email.Messages()) != 0 {
		t.Fatal("sent while failing")
	}
	f.email.SetErr(nil)
	// The retry waits for its backoff.
	if err := f.s.DeliverNotifications(ctx()); err != nil || len(f.email.Messages()) != 0 {
		t.Fatalf("retried before the backoff: %v", err)
	}
	f.clock.Advance(2 * time.Minute)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if len(f.email.Messages()) != 2 {
		t.Fatalf("emails after the retry: %d", len(f.email.Messages()))
	}
}

type sendError struct{}

func (sendError) Error() string { return "send failed" }

var errSendTest error = sendError{}

func TestCloseAccountCancelsSubscription(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.tx(func(tx pgx.Tx) error { return f.s.CloseAccount(ctx(), tx, acct) })
	sub, _ := f.paddle.Subscription(subID)
	if sub.Status != paddle.SubscriptionStatusCanceled {
		t.Fatalf("Paddle subscription %s", sub.Status)
	}
	stored, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if stored.Status != SubscriptionCanceled {
		t.Fatalf("stored status %s", stored.Status)
	}
	f.wantLedger(acct,
		wantEntry{LedgerManualTopUp, 25 * usd, 25 * usd},
		wantEntry{LedgerExpiry, -25 * usd, 0},
	)

	// When Paddle is unavailable the deletion fails and nothing changes.
	other := f.account()
	otherSub := f.activate(other)
	f.paddle.Fail("POST /subscriptions/", &paddletest.Failure{Status: http.StatusServiceUnavailable, Code: "unavailable"})
	err := f.db.InTx(ctx(), func(tx pgx.Tx) error { return f.s.CloseAccount(ctx(), tx, other) })
	if err == nil {
		t.Fatal("CloseAccount succeeded while Paddle is unavailable")
	}
	if s, _ := f.paddle.Subscription(otherSub); s.Status != paddle.SubscriptionStatusActive {
		t.Fatal("subscription changed")
	}
}

func TestWorkerWakeAndStop(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	runCtx, cancel := context.WithCancel(ctx())
	done := make(chan struct{})
	go func() {
		f.s.RunWorker(runCtx, time.Hour)
		close(done)
	}()
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("not queued")
	}
	f.s.Wake()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.paddle.Charges()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("Wake did not charge the queued top-up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunWorker did not stop")
	}
}

func TestLateEventDoesNotReplaceNewCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	oldSub := f.activate(acct)
	f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now()})
	txnID := f.checkout(acct)
	// A late, older event of the ended subscription arrives.
	f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now().Add(-time.Hour)})
	f.webhook("subscription.updated", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now().Add(time.Hour)})
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionPending || sub.CheckoutTransactionID != txnID {
		t.Fatalf("subscription = %+v", sub)
	}
	// The new subscription replaces the pending checkout.
	newSub := f.paddle.NewID("sub")
	f.webhook("subscription.created", map[string]any{"id": newSub, "status": "active", "transaction_id": txnID, "updated_at": f.clock.Now()})
	sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != newSub {
		t.Fatalf("subscription = %+v", sub)
	}
}

// TestLateActiveEventDoesNotReplaceNewCheckout checks that a late event of an
// earlier subscription, in any status, leaves the row of a new open checkout
// PENDING, also when its updated_at is after the cancellation, and that the
// subscription of the checkout transaction still completes it. A PENDING row
// without a checkout transaction accepts the first entity.
func TestLateActiveEventDoesNotReplaceNewCheckout(t *testing.T) {
	for _, status := range []string{
		paddle.SubscriptionStatusActive, paddle.SubscriptionStatusTrialing,
		paddle.SubscriptionStatusPastDue, paddle.SubscriptionStatusPaused,
	} {
		for _, afterCancel := range []bool{false, true} {
			name := status
			if afterCancel {
				name += " after the cancellation"
			}
			t.Run(name, func(t *testing.T) {
				f := newFixture(t)
				acct := f.account()
				f.credit(acct, 1000)
				oldSub := f.activate(acct)
				cancelTime := f.clock.Now()
				f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": cancelTime})
				txnID := f.checkout(acct)
				org := f.link(acct, "Acme")
				logs := f.captureLogs()

				updatedAt := cancelTime.Add(-time.Hour)
				if afterCancel {
					updatedAt = cancelTime.Add(time.Hour)
				}
				f.webhook("subscription.updated", map[string]any{"id": oldSub, "status": status, "updated_at": updatedAt})
				sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
				if sub.Status != SubscriptionPending || sub.CheckoutTransactionID != txnID || sub.PaddleSubscriptionID != "" {
					t.Fatalf("subscription after the late %s event = %+v", status, sub)
				}
				if got := f.status(org).Status; got == StatusAllowed {
					t.Fatalf("organization status = %s after the late event", got)
				}
				if !strings.Contains(logs.String(), "billing_subscription_entity_ignored") ||
					!strings.Contains(logs.String(), `"billing_account_id":"`+acct+`"`) ||
					!strings.Contains(logs.String(), `"paddle_subscription_id":"`+oldSub+`"`) {
					t.Fatalf("logs do not name the ignored entity: %s", logs)
				}
				// The open checkout is still returned, not already_exists.
				if again := f.checkout(acct); again != txnID {
					t.Fatalf("second StartSubscription = %s, want %s", again, txnID)
				}
				// The subscription of the checkout transaction completes it.
				newSub := f.paddle.NewID("sub")
				f.webhook("subscription.created", map[string]any{"id": newSub, "status": "active", "transaction_id": txnID, "updated_at": f.clock.Now()})
				sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
				if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != newSub || sub.CheckoutTransactionID != "" {
					t.Fatalf("subscription after subscription.created = %+v", sub)
				}
			})
		}
	}
}

// TestUpdateBeforeCreatedActivatesNewCheckout checks that an update of the
// new subscription that arrives before its subscription.created is accepted,
// because its ID was not recorded before.
func TestUpdateBeforeCreatedActivatesNewCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	oldSub := f.activate(acct)
	f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now()})
	f.checkout(acct)
	newSub := f.paddle.NewID("sub")
	f.webhook("subscription.updated", map[string]any{
		"id": newSub, "status": "active", "updated_at": f.clock.Now().Add(time.Minute),
		"custom_data": map[string]any{paddle.CustomDataBillingAccountID: acct},
	})
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != newSub {
		t.Fatalf("subscription = %+v", sub)
	}
}

// TestPendingWithoutCheckoutAcceptsFirstEntity checks that a PENDING row
// without a checkout transaction accepts the first entity of an earlier
// subscription.
func TestPendingWithoutCheckoutAcceptsFirstEntity(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.tx(func(tx pgx.Tx) error {
		return saveSubscription(ctx(), tx, acct, Subscription{Status: SubscriptionPending}, "", f.s.timestamp())
	})
	first := f.paddle.NewID("sub")
	f.webhook("subscription.updated", map[string]any{
		"id": first, "status": "active", "updated_at": f.clock.Now(),
		"custom_data": map[string]any{paddle.CustomDataBillingAccountID: acct},
	})
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != first {
		t.Fatalf("subscription = %+v", sub)
	}
}

// TestLatePaymentOfEarlierSubscriptionKeepsNewCheckout checks that the
// verified payment of a canceled earlier subscription neither activates nor
// alters the pending row of a new checkout, that the credit is still granted
// once, and that the payment of the checkout transaction activates it.
func TestLatePaymentOfEarlierSubscriptionKeepsNewCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	oldSub := f.activate(acct)
	f.webhook("subscription.updated", map[string]any{
		"id": oldSub, "status": "active", "updated_at": f.clock.Now(),
		"scheduled_change": map[string]any{"action": "cancel", "effective_at": f.clock.Now().Add(time.Hour)},
	})
	f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now().Add(time.Minute)})
	txnID := f.checkout(acct)
	before, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if before.Status != SubscriptionPending || before.CheckoutTransactionID != txnID {
		t.Fatalf("subscription before the payment: %+v", before)
	}

	late := paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_old_renewal", SubscriptionID: oldSub, Origin: paddle.OriginSubscriptionRecurring,
		BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000, PeriodEnd: f.clock.Now().AddDate(0, 1, 0),
	})
	body := paddletest.Event("evt_late_pay", paddle.EventTransactionCompleted, f.clock.Now(), late)
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	for range 3 {
		if code := f.deliver(body, sig); code != http.StatusOK {
			t.Fatalf("status %d", code)
		}
	}
	if code := f.webhook(paddle.EventTransactionCompleted, late); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	after, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if after.Status != SubscriptionPending || after.CheckoutTransactionID != txnID || after.PaddleSubscriptionID != "" ||
		after.CancelAtPeriodEnd != before.CancelAtPeriodEnd || after.CurrentPeriodEnd != nil {
		t.Fatalf("subscription after the late payment: %+v, before %+v", after, before)
	}

	// The payment of the checkout transaction activates the new subscription.
	newSub := f.paddle.NewID("sub")
	periodEnd := f.clock.Now().AddDate(0, 1, 0).Truncate(time.Second)
	if code := f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: newSub, Origin: paddle.OriginWeb, PriceID: priceID, SubtotalCents: 1000, PeriodEnd: periodEnd,
	})); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 20 * usd})
	after, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if after.Status != SubscriptionActive || after.PaddleSubscriptionID != newSub || after.CheckoutTransactionID != "" ||
		after.CancelAtPeriodEnd || after.CurrentPeriodEnd == nil || !after.CurrentPeriodEnd.Equal(periodEnd) {
		t.Fatalf("subscription after the checkout payment: %+v", after)
	}
}

// TestPaymentActivatesPendingRowWithoutCheckoutTransaction checks the case
// where the payment arrives before any checkout was recorded: the first
// verified subscription payment activates the pending row.
func TestPaymentActivatesPendingRowWithoutCheckoutTransaction(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	f.tx(func(tx pgx.Tx) error {
		return saveSubscription(ctx(), tx, acct, Subscription{Status: SubscriptionPending}, "", f.s.timestamp())
	})
	subID := f.paddle.NewID("sub")
	if code := f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_first", SubscriptionID: subID, Origin: paddle.OriginWeb, BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000,
	})); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != subID {
		t.Fatalf("subscription = %+v", sub)
	}
}
