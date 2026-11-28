// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

func failedRenewal(txnID, subID string) map[string]any {
	return paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: subID, Origin: paddle.OriginSubscriptionRecurring,
		PriceID: priceID, SubtotalCents: 1000, Status: "past_due",
	})
}

func (f *fixture) startReplacementCheckout(accountID, oldSub string) string {
	f.t.Helper()
	f.clock.Advance(2 * time.Minute)
	if code := f.webhook("subscription.canceled", map[string]any{
		"id": oldSub, "status": "canceled", "updated_at": f.clock.Now(),
	}); code != http.StatusOK {
		f.t.Fatalf("cancel original subscription: HTTP %d", code)
	}
	return f.checkout(accountID)
}

func (f *fixture) completeReplacementCheckout(txnID string) string {
	f.t.Helper()
	subID := f.paddle.NewID("sub")
	if code := f.webhook(paddle.EventTransactionCompleted, paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: subID, Origin: paddle.OriginWeb, PriceID: priceID, SubtotalCents: 1000,
	})); code != http.StatusOK {
		f.t.Fatalf("complete replacement checkout: HTTP %d", code)
	}
	return subID
}

func TestWebhookFailedRenewalRequiresCurrentSubscription(t *testing.T) {
	f := newFixture(t)
	accountID := f.account()
	oldSub := f.activate(accountID)
	failureTime := f.clock.Now()
	checkoutID := f.startReplacementCheckout(accountID, oldSub)
	assertIgnored := func(eventID, txnID string) {
		t.Helper()
		body := paddletest.Event(eventID, paddle.EventTransactionPaymentFailed, failureTime, failedRenewal(txnID, oldSub))
		if code := f.deliver(body, paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)); code != http.StatusOK {
			t.Fatalf("late renewal: HTTP %d", code)
		}
		var result, recordedAccount string
		if err := f.db.Pool.QueryRow(ctx(), `SELECT result, billing_account_id::text
			FROM paddle_events WHERE paddle_event_id = $1`, eventID).Scan(&result, &recordedAccount); err != nil {
			t.Fatal(err)
		}
		if result != webhookIgnored || recordedAccount != accountID {
			t.Errorf("late renewal recorded as (%q, %q), want ignored for %s", result, recordedAccount, accountID)
		}
	}
	assertIgnored("evt_failure_during_checkout", "txn_failure_during_checkout")
	currentSub := f.completeReplacementCheckout(checkoutID)
	assertIgnored("evt_failure_after_replacement", "txn_failure_after_replacement")

	sub, err := LoadSubscription(ctx(), f.db.Pool, accountID, false)
	if err != nil || sub.PaddleSubscriptionID != currentSub || sub.Status != SubscriptionActive {
		t.Fatalf("current subscription = %+v, err = %v", sub, err)
	}
	for range 2 {
		if got := f.webhookResult(paddle.EventTransactionPaymentFailed, failedRenewal("txn_current_failure", currentSub)); got != webhookApplied {
			t.Fatalf("current renewal result = %q, want %q", got, webhookApplied)
		}
	}
	var count int
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications
		WHERE billing_account_id = $1 AND kind = $2`, accountID, NotifyPaymentFailed).Scan(&count); err != nil || count != 1 {
		t.Fatalf("late and current failures queued %d notices, want 1, err = %v", count, err)
	}
	var subscriptionID string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT details->>'paddle_subscription_id' FROM billing_notifications
		WHERE dedup_key = 'payment_failed/subscription/txn_current_failure'`).Scan(&subscriptionID); err != nil || subscriptionID != currentSub {
		t.Fatalf("queued subscription = %q, want %q, err = %v", subscriptionID, currentSub, err)
	}

	f.s.email = &hookSender{next: f.email, before: func(_ context.Context, msg email.Message) error {
		if strings.HasPrefix(msg.To, "admin-") {
			return errSendTest
		}
		return nil
	}}
	f.s.RunOnce(ctx())
	f.s.email = f.email
	f.clock.Advance(2 * time.Minute)
	f.s.RunOnce(ctx())
	messages := f.email.Messages()
	if len(messages) != 2 || !strings.HasPrefix(messages[0].To, "owner-") || !strings.HasPrefix(messages[1].To, "admin-") {
		t.Fatalf("want one delivery each to owner and admin, got %+v", messages)
	}
	for _, msg := range messages {
		if msg.Kind != email.KindPaymentFailed || !strings.HasPrefix(msg.IdempotencyKey, "payment_failed/subscription/txn_current_failure/") {
			t.Errorf("unexpected delivery: kind = %q, key = %q", msg.Kind, msg.IdempotencyKey)
		}
	}
}

func TestFailedRenewalDeliveryRechecksSubscription(t *testing.T) {
	for _, name := range []string{"before attempt", "during lookup", "between attempts", "legacy"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			accountID := f.account()
			oldSub := f.activate(accountID)
			if got := f.webhookResult(paddle.EventTransactionPaymentFailed, failedRenewal("txn_queued_failure", oldSub)); got != webhookApplied {
				t.Fatalf("renewal result = %q, want %q", got, webhookApplied)
			}
			var id string
			if err := f.db.Pool.QueryRow(ctx(), `SELECT notification_id::text FROM billing_notifications
				WHERE dedup_key = 'payment_failed/subscription/txn_queued_failure'`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			replace := func() {
				f.completeReplacementCheckout(f.startReplacementCheckout(accountID, oldSub))
			}
			var prior noticeState
			lookups := 0
			switch name {
			case "during lookup":
				f.s.directory = notificationDirectoryFunc(func(ctx context.Context, userID string) (directory.User, error) {
					lookups++
					if lookups == 1 {
						replace()
					}
					return f.dir.User(ctx, userID)
				})
			case "between attempts":
				f.s.email = &hookSender{next: f.email, before: func(_ context.Context, msg email.Message) error {
					if strings.HasPrefix(msg.To, "owner-") {
						return errSendTest
					}
					return nil
				}}
				f.s.RunOnce(ctx())
				prior = f.noticeState(id)
				if prior.Attempts != 1 || prior.Sent || prior.Abandoned || len(prior.Recipients) != 1 || len(f.email.Messages()) != 1 {
					t.Fatalf("expected one recorded delivery after the first attempt, got %+v", prior)
				}
				f.s.email = f.email
				replace()
			case "legacy":
				if _, err := f.db.Pool.Exec(ctx(), `UPDATE billing_notifications
					SET details = details - 'paddle_subscription_id' WHERE notification_id = $1`, id); err != nil {
					t.Fatal(err)
				}
				replace()
			default:
				replace()
			}
			f.s.RunOnce(ctx())
			after := f.noticeState(id)
			sends := len(f.email.Messages())
			if name == "legacy" {
				if !after.Sent || after.Abandoned || len(after.Recipients) != 2 || sends != 2 {
					t.Fatalf("legacy notice: state %+v, sends %d", after, sends)
				}
				return
			}
			if !after.Abandoned || after.Sent || after.Attempts != prior.Attempts ||
				!slices.Equal(after.Recipients, prior.Recipients) || sends != len(prior.Recipients) {
				t.Fatalf("stale notice: state %+v, sends %d, prior state %+v", after, sends, prior)
			}
			if name == "during lookup" && lookups != 1 {
				t.Fatalf("recipient lookups = %d, want 1", lookups)
			}
		})
	}
}
