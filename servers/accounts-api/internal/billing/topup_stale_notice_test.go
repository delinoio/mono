// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/email"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// timedOutManualTopUp creates a manual top-up whose charge Paddle accepted and
// that then times out, which queues one payment-failed notification. It
// returns the account ID, the ID of that notification and the late webhook
// that completes the top-up.
func timedOutManualTopUp(t *testing.T) (f *fixture, acct, noticeID string, completion func()) {
	t.Helper()
	f = newFixture(t)
	acct = f.account()
	f.activate(acct)
	if err := f.manualTopUp(acct, topUpKey); err != nil {
		t.Fatalf("CreateTopUp: %v", err)
	}
	f.timeOutTopUps()
	wantStatuses(t, "after the timeout", f.topUpStatuses(acct), "failed:timed_out")
	if err := f.db.Pool.QueryRow(ctx(), `SELECT notification_id::text FROM billing_notifications
		WHERE billing_account_id = $1 AND kind = 'payment_failed'`, acct).Scan(&noticeID); err != nil {
		t.Fatalf("queued failure notification: %v", err)
	}
	charge := f.paddle.Charges()[0]
	completion = func() {
		t.Helper()
		body := paddletest.Event("evt_late_top_up", paddle.EventTransactionCompleted, f.clock.Now(),
			paddletest.Transaction(paddletest.TransactionData{
				ID: f.paddle.NewID("txn"), Origin: paddle.OriginSubscriptionCharge, TopUpID: paddletest.ChargeTopUpID(charge),
				SubtotalCents: 2500, TaxCents: 250,
			}))
		if code := f.deliver(body, paddle.SignatureHeaderValue(whSecret, f.clock.Now(), body)); code != http.StatusOK {
			t.Fatalf("webhook status %d", code)
		}
		wantStatuses(t, "after the late webhook", f.topUpStatuses(acct), "completed")
		f.wantCredit(acct, 1, 1)
	}
	return f, acct, noticeID, completion
}

func TestStaleTopUpFailureNoticeIsSuppressedOnFirstDelivery(t *testing.T) {
	f, _, noticeID, complete := timedOutManualTopUp(t)
	complete()

	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if msgs := f.email.Messages(); len(msgs) != 0 {
		t.Fatalf("emails after the completion = %+v, want none", msgs)
	}
	if st := f.noticeState(noticeID); !st.Abandoned || st.Sent || len(st.Recipients) != 0 {
		t.Fatalf("notification state = %+v, want abandoned and unsent", st)
	}
	// An abandoned notification is not picked up again.
	f.clock.Advance(2 * time.Hour)
	if err := f.s.DeliverNotifications(ctx()); err != nil || len(f.email.Messages()) != 0 {
		t.Fatalf("second pass: err %v, emails %d", err, len(f.email.Messages()))
	}
}

func TestStaleTopUpFailureNoticeIsSuppressedOnRetry(t *testing.T) {
	f, _, noticeID, complete := timedOutManualTopUp(t)

	// The first attempt fails for every recipient, so the notice waits for
	// its retry.
	f.email.SetErr(errSendTest)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(noticeID); st.Attempts != 1 || st.Sent || st.Abandoned {
		t.Fatalf("state after the failed attempt = %+v", st)
	}

	// The late webhook completes the top-up before the retry.
	complete()
	f.email.SetErr(nil)
	f.clock.Advance(2 * time.Minute)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if msgs := f.email.Messages(); len(msgs) != 0 {
		t.Fatalf("emails on the retry = %+v, want none", msgs)
	}
	if st := f.noticeState(noticeID); !st.Abandoned || st.Sent {
		t.Fatalf("state after the retry = %+v, want abandoned", st)
	}
}

// A notification queued by a release that did not record the top-up ID in its
// details still names the top-up in its deduplication key.
func TestStaleTopUpFailureNoticeWithoutTopUpIDDetail(t *testing.T) {
	f, _, noticeID, complete := timedOutManualTopUp(t)
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE billing_notifications SET details = details - 'top_up_id' WHERE notification_id = $1`, noticeID); err != nil {
		t.Fatal(err)
	}
	complete()

	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if msgs := f.email.Messages(); len(msgs) != 0 {
		t.Fatalf("emails = %+v, want none", msgs)
	}
	if st := f.noticeState(noticeID); !st.Abandoned {
		t.Fatalf("state = %+v, want abandoned", st)
	}
}

func TestFailedTopUpNoticeStillSends(t *testing.T) {
	f, _, noticeID, _ := timedOutManualTopUp(t)

	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	msgs := f.email.Messages()
	if len(msgs) == 0 {
		t.Fatal("a failed top-up sent no notice")
	}
	for _, m := range msgs {
		if m.Kind != email.KindPaymentFailed {
			t.Fatalf("email kind %q", m.Kind)
		}
	}
	if st := f.noticeState(noticeID); !st.Sent || st.Abandoned {
		t.Fatalf("state = %+v, want sent", st)
	}
}
