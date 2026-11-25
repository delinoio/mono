// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

func TestCreditExpiryNotificationRetriesOnlyBeforeExpiry(t *testing.T) {
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
			id := f.queueNotice(acct, "credit_expiry/retry")
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
			} else if after.Abandoned || !after.Sent || after.Attempts != 2 || len(after.Recipients) != 2 ||
				len(messages) != 2 || !strings.HasPrefix(messages[1].To, "admin-") {
				t.Fatalf("timely retry did not send only to the admin: state %+v, %d sends", after, len(messages))
			}
		})
	}
}

type notificationDirectoryFunc func(context.Context, string) (directory.User, error)

func (f notificationDirectoryFunc) User(ctx context.Context, userID string) (directory.User, error) {
	return f(ctx, userID)
}

func TestCreditExpiryNotificationRetiresDuringRecipientLookup(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	expiry := f.clock.Now().Add(24 * time.Hour)
	id := f.queueNotice(acct, "credit_expiry/lookup")
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
	f := newFixture(t)
	acct := f.account()
	id := f.queueNotice(acct, "credit_expiry/invalid")
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
