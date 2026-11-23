// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// renewal returns the completed payment of one billing period of subscription
// subID that ends at periodEnd. It has no custom data, so the recorded
// subscription finds the billing account.
func renewal(txnID, subID string, periodEnd time.Time) map[string]any {
	return paddletest.Transaction(paddletest.TransactionData{
		ID: txnID, SubscriptionID: subID, Origin: paddle.OriginSubscriptionRecurring,
		PriceID: priceID, SubtotalCents: 1000, PeriodEnd: periodEnd,
	})
}

// pastDueAtPeriod moves the subscription to PAST_DUE with the current period
// that ends at periodEnd, through a newer subscription entity.
func (f *fixture) pastDueAtPeriod(subID string, periodEnd time.Time) {
	f.t.Helper()
	if code := f.webhook("subscription.past_due", map[string]any{
		"id": subID, "status": "past_due", "updated_at": f.clock.Now().Add(time.Minute),
		"current_billing_period": map[string]any{"starts_at": periodEnd.AddDate(0, -1, 0), "ends_at": periodEnd},
	}); code != http.StatusOK {
		f.t.Fatalf("past_due webhook status %d", code)
	}
}

// TestLateRenewalPaymentKeepsNewerPastDueStatus checks that the payment of an
// older billing period, delivered after the newer past_due entity, earns its
// credit once but does not make the subscription ACTIVE, while the payment of
// the current period still recovers it.
func TestLateRenewalPaymentKeepsNewerPastDueStatus(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	currentEnd := f.clock.Now().AddDate(0, 2, 0).Truncate(time.Second)
	olderEnd := currentEnd.AddDate(0, -1, 0)
	f.pastDueAtPeriod(subID, currentEnd)

	assertPastDue := func(stage string) {
		t.Helper()
		sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
		if err != nil {
			t.Fatal(err)
		}
		if sub.Status != SubscriptionPastDue || sub.PaddleSubscriptionID != subID ||
			sub.CurrentPeriodEnd == nil || !sub.CurrentPeriodEnd.Equal(currentEnd) {
			t.Fatalf("%s: subscription %+v, want PAST_DUE of %s until %s", stage, sub, subID, currentEnd)
		}
	}
	assertPastDue("after the past_due entity")

	// The late payment of the older period, in a new event.
	late := renewal("txn_late_renewal", subID, olderEnd)
	if code := f.webhook(paddle.EventTransactionCompleted, late); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	assertPastDue("after the late payment")

	// A repeated delivery of the same transaction in another event adds no
	// credit and changes no status.
	if code := f.webhook(paddle.EventTransactionCompleted, late); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
	assertPastDue("after the replay")

	// The payment of the current period recovers the subscription.
	if code := f.webhook(paddle.EventTransactionCompleted, renewal("txn_current_renewal", subID, currentEnd)); code != http.StatusOK {
		t.Fatal(code)
	}
	f.wantLedger(acct,
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd},
		wantEntry{LedgerSubscriptionCredit, 10 * usd, 20 * usd})
	sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Status != SubscriptionActive || sub.CurrentPeriodEnd == nil || !sub.CurrentPeriodEnd.Equal(currentEnd) {
		t.Fatalf("subscription after the current payment: %+v", sub)
	}
}

// TestRenewalPaymentOutsideOlderPeriodStillRecoversPastDue checks the cases
// the older-period rule must not touch: a payment without a billing period, a
// payment of the same period as the stored one and a payment of a newer
// period all still activate a PAST_DUE subscription.
func TestRenewalPaymentOutsideOlderPeriodStillRecoversPastDue(t *testing.T) {
	currentEnd := time.Date(2030, time.March, 1, 0, 0, 0, 0, time.UTC)
	for name, periodEnd := range map[string]time.Time{
		"no billing period": {},
		"same period":       currentEnd,
		"newer period":      currentEnd.AddDate(0, 1, 0),
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			subID := f.activate(acct)
			f.pastDueAtPeriod(subID, currentEnd)
			if code := f.webhook(paddle.EventTransactionCompleted, renewal("txn_recovery", subID, periodEnd)); code != http.StatusOK {
				t.Fatal(code)
			}
			f.wantLedger(acct, wantEntry{LedgerSubscriptionCredit, 10 * usd, 10 * usd})
			sub, err := LoadSubscription(ctx(), f.db.Pool, acct, false)
			if err != nil {
				t.Fatal(err)
			}
			if sub.Status != SubscriptionActive {
				t.Fatalf("subscription after the payment: %+v, want ACTIVE", sub)
			}
		})
	}
}
