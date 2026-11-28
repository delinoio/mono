// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

// cancelSubscription delivers a fresh cancellation webhook and verifies that
// the billing account's subscription is canceled.
func (f *fixture) cancelSubscription(accountID, subID string) {
	f.t.Helper()
	f.clock.Advance(time.Minute)
	if code := f.webhook("subscription.canceled", map[string]any{"id": subID, "status": "canceled", "updated_at": f.clock.Now()}); code != http.StatusOK {
		f.t.Fatalf("subscription.canceled webhook status = %d, want %d", code, http.StatusOK)
	}
	sub, err := LoadSubscription(ctx(), f.db.Pool, accountID, false)
	if err != nil {
		f.t.Fatalf("load subscription after cancellation: %v", err)
	}
	if sub.PaddleSubscriptionID != subID || sub.Status != SubscriptionCanceled {
		f.t.Fatalf("stored subscription after cancellation: ID %q, status %q, want ID %q and status %q", sub.PaddleSubscriptionID, sub.Status, subID, SubscriptionCanceled)
	}
}

// A queued top-up that the current settings no longer allow closes as
// settings_changed even when the subscription was canceled in the meantime:
// the settings revalidation runs before the subscription check, so the top-up
// does not count as a failed charge and sends no email.
func TestPendingAutoTopUpClosedAsObsoleteWithCanceledSubscription(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	f.updateAutoTopUp(acct, false, 0, 0, 0)
	f.cancelSubscription(acct, subID)
	f.wantObsoleteTopUp(acct)
}

// When the settings still allow the top-up, a canceled subscription keeps
// failing it as no_subscription with the usual failure handling: the failed
// automatic top-up is counted, the owner and admins get the payment failure
// email and Paddle is not called.
func TestPendingAutoTopUpFailsWithCanceledSubscriptionWhenSettingsAllow(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	subID := f.activate(acct)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	if res := f.deduct(acct, usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	f.cancelSubscription(acct, subID)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if charges := f.paddle.Charges(); len(charges) != 0 {
		t.Fatalf("a top-up without a subscription called Paddle: %+v", charges)
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "failed:no_subscription" {
		t.Fatalf("top-ups = %v", got)
	}
	if st, _ := loadSettings(ctx(), f.db.Pool, acct); st.ConsecutiveFailures != 1 {
		t.Fatalf("settings = %+v, want one counted failure", st)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, m := range f.email.Messages() {
		if m.Kind == email.KindPaymentFailed {
			failed++
		}
	}
	if failed == 0 {
		t.Fatal("no payment failure email for the no_subscription failure")
	}
}
