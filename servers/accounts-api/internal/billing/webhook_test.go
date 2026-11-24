// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
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
	if code := f.webhook("subscription.updated", entity); code != http.StatusOK {
		t.Fatalf("subscription.updated status = %d, want %d", code, http.StatusOK)
	}
	stale := map[string]any{"id": subID, "status": "past_due", "updated_at": f.clock.Now()}
	// A stale delivery is acknowledged without changing the state; a 500 here
	// would make Paddle retry it.
	if code := f.webhook("subscription.past_due", stale); code != http.StatusOK {
		t.Fatalf("stale subscription.past_due status = %d, want %d", code, http.StatusOK)
	}
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

func TestPaddleWebhookPreservesSuspensionAfterAccountLock(t *testing.T) {
	testPaddleWebhookAcrossSuspensionDeadline(t, true)
}

func TestPaddleWebhookSuspendsAfterAccountLock(t *testing.T) {
	testPaddleWebhookAcrossSuspensionDeadline(t, false)
}

func testPaddleWebhookAcrossSuspensionDeadline(t *testing.T, workerSuspends bool) {
	t.Helper()
	f := newFixture(t)
	accountID := f.account()
	subID := f.activate(accountID)
	storedUpdate := f.clock.Now()
	if code := f.webhook("subscription.updated", paddle.Subscription{
		ID: subID, Status: paddle.SubscriptionStatusPastDue, UpdatedAt: storedUpdate,
	}); code != http.StatusOK {
		t.Fatalf("seed subscription webhook: status %d", code)
	}
	org := f.link(accountID, "Acme")
	blocked := f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	startedAt := blocked.SuspendTime.Add(-time.Microsecond)
	f.clock.Set(startedAt)

	updatedAt := storedUpdate.Add(time.Hour)
	wantResult := webhookApplied
	if workerSuspends {
		updatedAt = storedUpdate.Add(-time.Hour)
		wantResult = webhookIgnored
	}
	const eventID = "evt_waiting_for_account"
	body := paddletest.Event(eventID, "subscription.updated", startedAt, paddle.Subscription{
		ID: subID, Status: paddle.SubscriptionStatusPastDue, UpdatedAt: updatedAt,
	})
	signature := paddle.SignatureHeaderValue(whSecret, startedAt, body)
	response := make(chan int, 1)
	wantSequence := blocked.Sequence + 1
	f.tx(func(tx pgx.Tx) error {
		if err := LockAccount(ctx(), tx, accountID); err != nil {
			return err
		}
		go func() {
			response <- f.deliver(body, signature)
		}()
		f.waitForAccountLockWait()
		f.clock.Set(*blocked.SuspendTime)
		if workerSuspends {
			// Use the holding transaction to record the suspension before
			// the waiting webhook can acquire the account lock.
			if err := f.s.SyncStatuses(ctx(), tx, accountID); err != nil {
				return err
			}
			suspended, ok, err := CurrentStatus(ctx(), tx, "ownsand", org)
			if err != nil || !ok || suspended.Status != StatusSuspended || suspended.Sequence != wantSequence {
				t.Fatalf("worker suspension: %+v, found %v, error %v", suspended, ok, err)
			}
		}
		return nil
	})
	if code := <-response; code != http.StatusOK {
		t.Fatalf("waiting subscription webhook: status %d", code)
	}

	var result string
	var processTime time.Time
	if err := f.db.Pool.QueryRow(ctx(), `SELECT result, process_time FROM paddle_events WHERE paddle_event_id = $1`,
		eventID).Scan(&result, &processTime); err != nil {
		t.Fatal(err)
	}
	if result != wantResult || !processTime.Equal(startedAt) {
		t.Fatalf("webhook result %s at %s, want %s at %s", result, processTime, wantResult, startedAt)
	}
	suspended := f.wantStatus(org, StatusSuspended, blocked.Reason)
	if !sameTime(suspended.BlockedSince, blocked.BlockedSince) || !sameTime(suspended.SuspendTime, blocked.SuspendTime) ||
		suspended.Sequence != wantSequence {
		t.Fatalf("suspension after webhook: %+v, want block start %v, deadline %v and sequence %d",
			suspended, blocked.BlockedSince, blocked.SuspendTime, wantSequence)
	}

	renewal := paddletest.Event("evt_renewal", paddle.EventTransactionCompleted, f.clock.Now(),
		paddletest.Transaction(paddletest.TransactionData{
			ID: "txn_renewal", SubscriptionID: subID, Origin: paddle.OriginSubscriptionRecurring,
			PriceID: priceID, SubtotalCents: 1000, PeriodEnd: f.clock.Now().AddDate(0, 1, 0),
		}))
	renewalSignature := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), renewal)
	if code := f.deliver(renewal, renewalSignature); code != http.StatusOK {
		t.Fatalf("renewal webhook: status %d", code)
	}
	sub, err := LoadSubscription(ctx(), f.db.Pool, accountID, false)
	if err != nil || sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != subID {
		t.Fatalf("subscription after renewal: %+v, error %v", sub, err)
	}
	allowed := f.wantStatus(org, StatusAllowed, ReasonNone)
	if allowed.Sequence != suspended.Sequence+1 {
		t.Fatalf("sequence after renewal = %d, want %d", allowed.Sequence, suspended.Sequence+1)
	}
	if code := f.deliver(renewal, renewalSignature); code != http.StatusOK {
		t.Fatalf("renewal replay: status %d", code)
	}
	if replay := f.wantStatus(org, StatusAllowed, ReasonNone); replay.Sequence != allowed.Sequence {
		t.Fatalf("sequence after renewal replay = %d, want %d", replay.Sequence, allowed.Sequence)
	}
	f.wantLedger(accountID, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
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

// emailsTo counts the recorded emails of one recipient address.
func emailsTo(f *fixture, addr string) int {
	n := 0
	for _, m := range f.email.Messages() {
		if m.To == addr {
			n++
		}
	}
	return n
}

// TestEmailDeliveryContinuesAfterRecipientFailure pins that one recipient
// that keeps failing does not keep a later recipient from getting the
// email, and that the notification is still abandoned after the retry
// budget.
func TestEmailDeliveryContinuesAfterRecipientFailure(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	// Recipients sort by user ID: "admin-N" comes before "owner-N".
	var adminID, ownerID string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT
		(SELECT user_id FROM billing_account_members WHERE billing_account_id = $1 AND role = 'admin'),
		(SELECT user_id FROM billing_account_members WHERE billing_account_id = $1 AND role = 'owner')`,
		acct).Scan(&adminID, &ownerID); err != nil {
		t.Fatal(err)
	}
	if adminID >= ownerID {
		t.Fatalf("admin %q must sort before owner %q", adminID, ownerID)
	}
	adminAddr, ownerAddr := adminID+"@example.com", ownerID+"@example.com"
	f.grant(acct, LedgerManualTopUp, 25*usd)
	f.deduct(acct, 20*usd)

	// The first sorted recipient always fails, the second succeeds.
	f.email.SetErrFor(adminAddr, errSendTest)
	for i := 0; i < notificationMaxAttempts+2; i++ {
		if err := f.s.DeliverNotifications(ctx()); err != nil {
			t.Fatal(err)
		}
		f.clock.Advance(time.Hour)
	}
	if got := emailsTo(f, ownerAddr); got != 1 {
		t.Fatalf("emails to the second recipient = %d, want exactly 1", got)
	}
	if got := emailsTo(f, adminAddr); got != 0 {
		t.Fatalf("emails to the failing recipient = %d", got)
	}
	var attempts int
	var abandoned bool
	var delivered int
	if err := f.db.Pool.QueryRow(ctx(), `SELECT n.attempts, n.abandon_time IS NOT NULL,
			(SELECT count(*) FROM billing_notification_deliveries d WHERE d.notification_id = n.notification_id)
		FROM billing_notifications n WHERE n.billing_account_id = $1 AND n.kind = $2`,
		acct, string(NotifyLowBalance)).Scan(&attempts, &abandoned, &delivered); err != nil {
		t.Fatal(err)
	}
	if attempts != notificationMaxAttempts || !abandoned || delivered != 1 {
		t.Fatalf("attempts %d abandoned %v deliveries %d", attempts, abandoned, delivered)
	}

	// A transient failure of the first recipient delivers to both after the
	// retry, and the second recipient is not emailed twice.
	acct2 := f.account()
	var admin2 string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT user_id FROM billing_account_members
		WHERE billing_account_id = $1 AND role = 'admin'`, acct2).Scan(&admin2); err != nil {
		t.Fatal(err)
	}
	f.grant(acct2, LedgerManualTopUp, 25*usd)
	f.deduct(acct2, 20*usd)
	f.email.SetErrFor(admin2+"@example.com", errSendTest)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	f.email.SetErrFor(admin2+"@example.com", nil)
	f.clock.Advance(2 * time.Minute)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := emailsTo(f, admin2+"@example.com"); got != 1 {
		t.Fatalf("emails to the recovered recipient = %d, want 1", got)
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
	// An early fatal assertion must stop the worker and wait for it before
	// the fixture closes the database and the provider, which t.Cleanup runs
	// in reverse order of registration. Cancel is idempotent, so the normal
	// path below may stop the worker first.
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("RunWorker did not stop during cleanup")
		}
	})
	// RunWorker reads the wake channel only after the startup RunOnce scan has
	// returned. Send a wake with nothing queued and wait until the worker has
	// received it: the drained channel proves the startup scan has finished,
	// so the startup scan cannot be what charges the top-up queued below. The
	// wake channel holds one signal and the idle wake handler does nothing.
	f.s.Wake()
	deadline := time.Now().Add(10 * time.Second)
	for len(f.s.wake) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the worker did not finish its startup scan")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("not queued")
	}
	if n := len(f.paddle.Charges()); n != 0 {
		t.Fatalf("%d charges before Wake", n)
	}
	f.s.Wake()
	deadline = time.Now().Add(10 * time.Second)
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
// subscription of the checkout transaction still completes it.
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

// webhookResult delivers a signed webhook event, requires HTTP 200 and returns
// the result stored for the event.
func (f *fixture) webhookResult(eventType string, data any) string {
	f.t.Helper()
	eventID := fmt.Sprintf("evt_result_%d", txnSeq.Add(1))
	body := paddletest.Event(eventID, eventType, f.clock.Now(), data)
	if code := f.deliver(body, paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)); code != http.StatusOK {
		f.t.Fatalf("webhook %s status = %d", eventType, code)
	}
	var result string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT result FROM paddle_events WHERE paddle_event_id = $1`, eventID).Scan(&result); err != nil {
		f.t.Fatal(err)
	}
	return result
}

// canceledReplacement ends subscription A, completes a new checkout with
// subscription B and cancels B at cancelTime. It returns A, B and the
// canceled row of B.
func (f *fixture) canceledReplacement(acct string, cancelTime time.Time) (oldSub, newSub string, sub Subscription) {
	f.t.Helper()
	oldSub = f.activate(acct)
	f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": cancelTime.Add(-2 * time.Hour)})
	txnID := f.checkout(acct)
	newSub = f.paddle.NewID("sub")
	f.webhook("subscription.created", map[string]any{"id": newSub, "status": "active", "transaction_id": txnID, "updated_at": cancelTime.Add(-time.Hour)})
	f.webhook("subscription.canceled", map[string]any{"id": newSub, "status": "canceled", "updated_at": cancelTime})
	sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionCanceled || sub.PaddleSubscriptionID != newSub {
		f.t.Fatalf("subscription after the cancellation of the new subscription = %+v", sub)
	}
	return oldSub, newSub, sub
}

// TestOlderEntityOfEarlierSubscriptionDoesNotReplaceCanceledSubscription
// checks that a late entity of an earlier subscription A, with a provider
// updated_at before the cancellation of the later subscription B, leaves the
// canceled row of B and its update time in place, is recorded as ignored and
// lets the next StartSubscription create a checkout.
func TestOlderEntityOfEarlierSubscriptionDoesNotReplaceCanceledSubscription(t *testing.T) {
	for _, status := range []string{paddle.SubscriptionStatusActive, paddle.SubscriptionStatusPastDue} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			cancelTime := f.clock.Now()
			oldSub, newSub, before := f.canceledReplacement(acct, cancelTime)
			if before.PaddleUpdateTime == nil || !before.PaddleUpdateTime.Equal(cancelTime) {
				t.Fatalf("update time of the canceled row = %v, want %v", before.PaddleUpdateTime, cancelTime)
			}

			got := f.webhookResult("subscription.updated", map[string]any{"id": oldSub, "status": status, "updated_at": cancelTime.Add(-30 * time.Minute)})
			if got != webhookIgnored {
				t.Fatalf("event result = %q, want %q", got, webhookIgnored)
			}
			sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if sub.Status != SubscriptionCanceled || sub.PaddleSubscriptionID != newSub ||
				sub.PaddleUpdateTime == nil || !sub.PaddleUpdateTime.Equal(cancelTime) {
				t.Fatalf("subscription after the older entity = %+v", sub)
			}
			// The next authorized StartSubscription creates a checkout.
			txnID := f.checkout(acct)
			sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
			if txnID == "" || sub.Status != SubscriptionPending || sub.CheckoutTransactionID != txnID {
				t.Fatalf("subscription after StartSubscription = %+v (transaction %q)", sub, txnID)
			}
		})
	}
}

// TestNewerEntityAfterCancellationStillApplies checks that an entity whose
// updated_at is after the cancellation, or equal to it, or missing, still
// replaces a canceled subscription of a different ID.
func TestNewerEntityAfterCancellationStillApplies(t *testing.T) {
	cases := map[string]func(cancelTime time.Time) any{
		"newer":   func(c time.Time) any { return c.Add(time.Minute) },
		"equal":   func(c time.Time) any { return c },
		"missing": func(time.Time) any { return nil },
	}
	for name, updatedAt := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			cancelTime := f.clock.Now()
			oldSub, _, _ := f.canceledReplacement(acct, cancelTime)
			entity := map[string]any{"id": oldSub, "status": "active"}
			if v := updatedAt(cancelTime); v != nil {
				entity["updated_at"] = v
			}
			if got := f.webhookResult("subscription.updated", entity); got != webhookApplied {
				t.Fatalf("event result = %q, want %q", got, webhookApplied)
			}
			sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != oldSub {
				t.Fatalf("subscription = %+v", sub)
			}
		})
	}
}

// TestDifferentSubscriptionDoesNotReplaceManageableRow checks that a
// different subscription ID cannot replace an ACTIVE or PAST_DUE row, also
// with a later updated_at, and that an older entity of the same ID is still
// ignored.
func TestDifferentSubscriptionDoesNotReplaceManageableRow(t *testing.T) {
	for _, status := range []string{paddle.SubscriptionStatusActive, paddle.SubscriptionStatusPastDue} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			cur := f.activate(acct)
			f.webhook("subscription.updated", map[string]any{"id": cur, "status": status, "updated_at": f.clock.Now()})
			// A subscription event binds the billing account only through a
			// recorded subscription, never through custom data, so the other
			// subscription is recorded first, as a late payment of it would.
			other := f.paddle.NewID("sub")
			f.tx(func(tx pgx.Tx) error {
				return recordPaddleSubscription(ctx(), tx, other, acct, f.s.timestamp())
			})
			if got := f.webhookResult("subscription.updated", map[string]any{
				"id": other, "status": "active", "updated_at": f.clock.Now().Add(time.Hour),
			}); got != webhookIgnored {
				t.Fatalf("event result = %q, want %q", got, webhookIgnored)
			}
			// An older entity of the same ID is still ignored.
			if got := f.webhookResult("subscription.canceled", map[string]any{"id": cur, "status": "canceled", "updated_at": f.clock.Now().Add(-time.Hour)}); got != webhookIgnored {
				t.Fatalf("event result = %q, want %q", got, webhookIgnored)
			}
			sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if !sub.Manageable() || sub.PaddleSubscriptionID != cur {
				t.Fatalf("subscription = %+v", sub)
			}
		})
	}
}

// TestUpdateBeforeCreatedActivatesNewCheckout checks that an update of the
// new subscription that arrives before its subscription.created matches only
// through custom data and so leaves the checkout PENDING, and that the
// subscription.created event of the checkout transaction completes it.
func TestUpdateBeforeCreatedActivatesNewCheckout(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	oldSub := f.activate(acct)
	f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now()})
	txnID := f.checkout(acct)
	newSub := f.paddle.NewID("sub")
	f.webhook("subscription.updated", map[string]any{
		"id": newSub, "status": "active", "updated_at": f.clock.Now().Add(time.Minute),
		"custom_data": map[string]any{paddle.CustomDataBillingAccountID: acct},
	})
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionPending || sub.CheckoutTransactionID != txnID || sub.PaddleSubscriptionID != "" {
		t.Fatalf("subscription after the early update = %+v", sub)
	}
	f.webhook("subscription.created", map[string]any{
		"id": newSub, "status": "active", "transaction_id": txnID, "updated_at": f.clock.Now().Add(time.Minute),
	})
	sub, _ = LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionActive || sub.PaddleSubscriptionID != newSub || sub.CheckoutTransactionID != "" {
		t.Fatalf("subscription after subscription.created = %+v", sub)
	}
}

// TestPendingWithoutCheckoutAcceptsFirstEntity checks that a PENDING row
// without a checkout transaction does not accept an entity that matches only
// through custom data.
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
	if sub.Status != SubscriptionPending || sub.PaddleSubscriptionID != "" {
		t.Fatalf("subscription = %+v", sub)
	}
	if n := f.count(`SELECT count(*) FROM paddle_subscriptions WHERE paddle_subscription_id = $1`, first); n != 0 {
		t.Fatalf("paddle_subscriptions has %d rows for the unbound subscription", n)
	}
}

// TestCustomDataDoesNotBindSubscription checks that custom data, which a
// checkout opened in the browser can set to any billing account, never binds
// a subscription: a subscription event whose transaction and subscription
// IDs the server did not record changes no subscription row, records no
// subscription and is stored as unmatched.
func TestCustomDataDoesNotBindSubscription(t *testing.T) {
	for _, name := range []string{"NONE", "CANCELED", "PENDING"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			pendingTxn := ""
			switch name {
			case "CANCELED":
				oldSub := f.activate(acct)
				f.webhook("subscription.canceled", map[string]any{"id": oldSub, "status": "canceled", "updated_at": f.clock.Now()})
			case "PENDING":
				pendingTxn = f.checkout(acct)
			}
			before, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
			logs := f.captureLogs()

			forged := f.paddle.NewID("sub")
			forgedTxn := f.paddle.NewID("txn")
			if code := f.webhook("subscription.created", map[string]any{
				"id": forged, "status": "active", "transaction_id": forgedTxn, "updated_at": f.clock.Now().Add(time.Minute),
				"custom_data": map[string]any{paddle.CustomDataBillingAccountID: acct},
			}); code != http.StatusOK {
				t.Fatal(code)
			}
			after, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("subscription changed from %+v to %+v", before, after)
			}
			if n := f.count(`SELECT count(*) FROM paddle_subscriptions WHERE paddle_subscription_id = $1`, forged); n != 0 {
				t.Fatalf("paddle_subscriptions has %d rows for the unbound subscription", n)
			}
			if n := f.count(`SELECT count(*) FROM paddle_events WHERE result = 'unmatched' AND billing_account_id IS NULL`); n != 1 {
				t.Fatalf("%d unmatched events without a billing account, want 1", n)
			}
			if !strings.Contains(logs.String(), "billing_subscription_unbound") ||
				!strings.Contains(logs.String(), `"paddle_subscription_id":"`+forged+`"`) ||
				!strings.Contains(logs.String(), `"paddle_transaction_id":"`+forgedTxn+`"`) ||
				!strings.Contains(logs.String(), `"billing_account_id":"`+acct+`"`) {
				t.Fatalf("logs do not name the unbound subscription: %s", logs)
			}
			// The victim can still start a subscription.
			again := f.checkout(acct)
			if name == "PENDING" && again != pendingTxn {
				t.Fatalf("second StartSubscription = %s, want the open checkout %s", again, pendingTxn)
			}
			if again == "" {
				t.Fatal("StartSubscription returned no checkout")
			}
		})
	}
}

// TestCustomDataPaymentOnlyAddsCredit checks that a subscription payment that
// matches only through custom data adds its credit once, but neither records
// nor activates its subscription, and that its failed renewal sends no email.
func TestCustomDataPaymentOnlyAddsCredit(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	logs := f.captureLogs()
	forgedSub := f.paddle.NewID("sub")
	payment := paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_forged", SubscriptionID: forgedSub, Origin: paddle.OriginWeb, BillingAccountID: acct,
		PriceID: priceID, SubtotalCents: 1000,
	})
	for range 2 { // Paddle retries: the credit is added once
		if code := f.webhook(paddle.EventTransactionCompleted, payment); code != http.StatusOK {
			t.Fatal(code)
		}
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	sub, _ := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if sub.Status != SubscriptionNone || sub.PaddleSubscriptionID != "" {
		t.Fatalf("subscription = %+v", sub)
	}
	if n := f.count(`SELECT count(*) FROM paddle_subscriptions WHERE paddle_subscription_id = $1`, forgedSub); n != 0 {
		t.Fatalf("paddle_subscriptions has %d rows for the unbound subscription", n)
	}
	if !strings.Contains(logs.String(), "billing_subscription_payment_unbound") {
		t.Fatalf("logs do not name the unbound payment: %s", logs)
	}
	// StartSubscription still succeeds.
	if f.checkout(acct) == "" {
		t.Fatal("StartSubscription returned no checkout")
	}

	// A failed renewal that matches only through custom data sends no email.
	f.webhook(paddle.EventTransactionPaymentFailed, paddletest.Transaction(paddletest.TransactionData{
		ID: "txn_forged_renew", SubscriptionID: f.paddle.NewID("sub"), Origin: paddle.OriginSubscriptionRecurring,
		BillingAccountID: acct, PriceID: priceID, SubtotalCents: 1000, Status: "past_due",
	}))
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if msgs := f.email.Messages(); len(msgs) != 0 {
		t.Fatalf("emails %+v", msgs)
	}
}

func TestLatePaymentOfEarlierSubscriptionKeepsCanceledReplacement(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	org := f.link(acct, "Organization")
	cancelTime := f.clock.Now()
	oldSub, _, before := f.canceledReplacement(acct, cancelTime)
	if before.PaddleUpdateTime == nil || !before.PaddleUpdateTime.Equal(cancelTime) {
		t.Fatalf("canceled replacement update time = %v, want %v", before.PaddleUpdateTime, cancelTime)
	}
	f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	logs := f.captureLogs()
	eventsBefore := f.eventCount()

	lateTxnID := f.paddle.NewID("txn")
	data := paddletest.Transaction(paddletest.TransactionData{
		ID: lateTxnID, SubscriptionID: oldSub, Origin: paddle.OriginSubscriptionRecurring,
		PriceID: priceID, SubtotalCents: 1000, PeriodEnd: f.clock.Now().AddDate(0, 1, 0),
	})
	body := paddletest.Event("evt_late_renewal", paddle.EventTransactionCompleted, f.clock.Now(), data)
	sig := paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)
	for range 2 {
		if code := f.deliver(body, sig); code != http.StatusOK {
			t.Fatalf("late payment webhook status = %d, want %d", code, http.StatusOK)
		}
		f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
		after, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatalf("subscription after the late payment = %+v, %v, want %+v", after, err, before)
		}
		f.wantStatus(org, StatusBlocked, ReasonNoActiveSubscription)
	}
	if got := f.eventCount(); got != eventsBefore+1 {
		t.Fatalf("recorded events = %d, want %d", got, eventsBefore+1)
	}
	if got := f.count(`SELECT count(*) FROM paddle_subscriptions
		WHERE paddle_subscription_id = $1 AND billing_account_id = $2`, oldSub, acct); got != 1 {
		t.Fatalf("mapped subscriptions for the renewal = %d, want 1", got)
	}
	var ignoredLogs []string
	for line := range strings.Lines(logs.String()) {
		if strings.Contains(line, `"msg":"billing_subscription_payment_ignored"`) {
			ignoredLogs = append(ignoredLogs, line)
		}
	}
	if len(ignoredLogs) != 1 ||
		!strings.Contains(ignoredLogs[0], `"billing_account_id":"`+acct+`"`) ||
		!strings.Contains(ignoredLogs[0], `"paddle_subscription_id":"`+oldSub+`"`) ||
		!strings.Contains(ignoredLogs[0], `"paddle_transaction_id":"`+lateTxnID+`"`) ||
		!strings.Contains(ignoredLogs[0], `"reason":"different_canceled_subscription"`) {
		t.Fatalf("ignored activation logs = %q, want one with the account, subscription, transaction, and reason fields", ignoredLogs)
	}

	txnID := f.checkout(acct)
	pending, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if err != nil || txnID == "" || pending.Status != SubscriptionPending || pending.CheckoutTransactionID != txnID {
		t.Fatalf("subscription after StartSubscription = %+v, %v (transaction %q)", pending, err, txnID)
	}
	newSub := f.paddle.NewID("sub")
	if code := f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: newSub, Origin: paddle.OriginWeb, PriceID: priceID, SubtotalCents: 1000,
	})); code != http.StatusOK {
		t.Fatalf("checkout payment webhook status = %d, want %d", code, http.StatusOK)
	}
	active, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if err != nil || active.Status != SubscriptionActive || active.PaddleSubscriptionID != newSub || active.CheckoutTransactionID != "" {
		t.Fatalf("subscription after the checkout payment = %+v, %v, want active subscription %q", active, err, newSub)
	}
	f.wantStatus(org, StatusAllowed, ReasonNone)
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
// where the payment arrives before any checkout was recorded: a payment that
// matches only through custom data adds its credit but leaves the pending
// row PENDING.
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
	if sub.Status != SubscriptionPending || sub.PaddleSubscriptionID != "" {
		t.Fatalf("subscription = %+v", sub)
	}
	if n := f.count(`SELECT count(*) FROM paddle_subscriptions WHERE paddle_subscription_id = $1`, subID); n != 0 {
		t.Fatalf("paddle_subscriptions has %d rows for the unbound subscription", n)
	}
}
