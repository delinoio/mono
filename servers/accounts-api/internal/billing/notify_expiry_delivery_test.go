// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

func TestCreditExpiryNotificationRetriesOnlyBeforeExpiry(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		offset     time.Duration
		wantRetire bool
	}{
		{name: "before expiry", offset: -time.Minute},
		{name: "at expiry", wantRetire: true},
		{name: "after expiry", offset: time.Second, wantRetire: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			expiry := f.clock.Now().Add(24 * time.Hour)
			id, _ := f.queueNotice(acct)
			f.s.email = &hookSender{next: f.email, before: func(_ context.Context, msg email.Message) error {
				if strings.HasPrefix(msg.To, "admin-") {
					return errors.New("send failed")
				}
				return nil
			}}

			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			first := f.noticeState(id)
			if first.Attempts != 1 || first.Sent || first.Abandoned || len(first.Recipients) != 1 ||
				!strings.HasPrefix(first.Recipients[0], "owner-") {
				t.Fatalf("initial attempt did not retain the owner delivery: %+v", first)
			}

			f.s.email = f.email
			f.clock.Set(expiry.Add(tc.offset))
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			after := f.noticeState(id)
			messages := f.email.Messages()
			if tc.wantRetire {
				if !after.Abandoned || after.Sent || after.Attempts != first.Attempts ||
					!slices.Equal(after.Recipients, first.Recipients) || len(messages) != 1 {
					t.Fatalf("expired retry changed delivery progress: state %+v, %d sends", after, len(messages))
				}
				return
			}
			if after.Abandoned || !after.Sent || after.Attempts != 2 || len(after.Recipients) != 2 ||
				len(messages) != 2 || !strings.HasPrefix(messages[1].To, "admin-") {
				t.Fatalf("timely retry did not send only to the admin: state %+v, %d sends", after, len(messages))
			}
			assertCreditExpiryMessage(t, messages[1], "$5.00 of credit expires on May 11, 2026 12:00 UTC.")
		})
	}
}

func assertCreditExpiryMessage(t *testing.T, msg email.Message, wantBody string) {
	t.Helper()
	if msg.Kind != email.KindCreditExpiry || msg.Subject != "Your Delino credit expires soon" {
		t.Errorf("expiry notice kind = %q, subject = %q", msg.Kind, msg.Subject)
	}
	if !strings.Contains(msg.Text, wantBody) || !strings.Contains(msg.HTML, wantBody) {
		t.Errorf("expiry notice bodies should both contain %q; text = %q, HTML = %q", wantBody, msg.Text, msg.HTML)
	}
}

func TestCreditExpiryNotificationRetriesAcrossSubjectChange(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	account := f.account()
	id, key := f.queueNotice(account)
	first, second := f.resendRecipients(account)
	firstKey, secondKey := key+"/"+first, key+"/"+second
	provider := &notificationProvider{responses: map[string][]providerResponse{
		secondKey: {{http.StatusServiceUnavailable, `{"message":"temporarily unavailable"}`}},
	}}
	logs := f.useNotificationProvider(provider)
	resend := f.s.email
	f.s.email = &hookSender{next: resend, before: func(ctx context.Context, msg email.Message) error {
		if msg.IdempotencyKey != firstKey {
			return nil
		}
		msg.Subject = "Delino credit expires in 7 days"
		if err := resend.Send(ctx, msg); err != nil {
			t.Fatalf("send old subject: %v", err)
		}
		// Simulate a lost acknowledgement after the provider accepts the old subject.
		return errors.New("Resend acknowledgement lost")
	}}

	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(id); st.Sent || st.Abandoned || st.Attempts != 1 || len(st.Recipients) != 0 {
		t.Fatalf("state after the failed pass = %+v", st)
	}

	f.s.email = resend
	f.clock.Advance(time.Minute)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(id); !st.Sent || st.Abandoned || st.Attempts != 2 ||
		!slices.Equal(st.Recipients, []string{first, second}) {
		t.Fatalf("state after the retry = %+v", st)
	}
	if !strings.Contains(logs.String(), `"reason":"idempotency_payload_conflict"`) {
		t.Fatal("the retry should record the provider payload conflict")
	}
	provider.assertKeys(t, firstKey, secondKey, firstKey, secondKey)

	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.accepted) != 2 ||
		!strings.Contains(string(provider.accepted[firstKey].payload), "Delino credit expires in 7 days") ||
		!strings.Contains(string(provider.accepted[secondKey].payload), "Your Delino credit expires soon") {
		t.Fatal("the provider should accept one message per recipient and keep the first payload")
	}
}

type notificationDirectoryFunc func(context.Context, string) (directory.User, error)

func (f notificationDirectoryFunc) User(ctx context.Context, userID string) (directory.User, error) {
	return f(ctx, userID)
}

func TestCreditExpiryNotificationRetiresDuringRecipientLookup(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	expiry := f.clock.Now().Add(24 * time.Hour)
	id, _ := f.queueNotice(acct)
	lookups := 0
	f.s.directory = notificationDirectoryFunc(func(ctx context.Context, userID string) (directory.User, error) {
		lookups++
		f.clock.Set(expiry)
		return f.dir.User(ctx, userID)
	})

	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	after := f.noticeState(id)
	if !after.Abandoned || after.Sent || after.Attempts != 0 || len(after.Recipients) != 0 ||
		len(f.email.Messages()) != 0 || lookups != 1 {
		t.Fatalf("crossing expiry did not stop delivery: state %+v, %d sends, %d lookups",
			after, len(f.email.Messages()), lookups)
	}
}

func TestCreditExpiryNotificationRetiresWithInvalidExpiry(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	acct := f.account()
	id, _ := f.queueNotice(acct)
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE billing_notifications SET details = '{"expire_time":"invalid"}'
		WHERE notification_id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	after := f.noticeState(id)
	if !after.Abandoned || after.Sent || len(f.email.Messages()) != 0 {
		t.Fatalf("invalid expiry did not stop delivery: state %+v, %d sends", after, len(f.email.Messages()))
	}
}
