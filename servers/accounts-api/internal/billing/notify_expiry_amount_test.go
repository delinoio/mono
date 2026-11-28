// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
)

func expiryAmountFixture(t *testing.T) (f *fixture, acct, grant, id string) {
	t.Helper()
	f = newFixture(t)
	acct = f.account()
	f.execResendSQL(`INSERT INTO credit_settings (billing_account_id, low_balance_threshold_usd_micros, update_time)
		VALUES ($1, 0, $2)`, acct, f.clock.Now())
	grant, id = f.queueExpiryNotice(acct, 10*usd, f.clock.Now().Add(6*24*time.Hour))
	return
}

func TestCreditExpiryNotificationReadsCommittedRemainder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		spent int64
		want  string
	}{
		{"unused", 0, "$10.00"},
		{"partly spent", 4 * usd, "$6.00"},
		{"fully spent", 10 * usd, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, acct, grant, id := expiryAmountFixture(t)
			f.deduct(acct, tc.spent)
			for range 2 {
				if err := f.s.QueueExpiryNotices(ctx()); err != nil {
					t.Fatal(err)
				}
				if err := f.s.DeliverNotifications(ctx()); err != nil {
					t.Fatal(err)
				}
			}
			st, msgs := f.noticeState(id), f.email.Messages()
			if tc.want == "" {
				if !st.Abandoned || st.Sent || len(msgs) != 0 {
					t.Fatalf("spent grant: state %+v, got %d sends", st, len(msgs))
				}
			} else {
				if !st.Sent || st.Abandoned || len(msgs) != 2 {
					t.Fatalf("usable grant: state %+v, got %d sends", st, len(msgs))
				}
				for _, msg := range msgs {
					if !strings.Contains(msg.Text, tc.want+" of credit expires") || strings.HasPrefix(msg.To, "viewer-") {
						t.Fatalf("unexpected expiry email to %q: %s", msg.To, msg.Text)
					}
				}
			}
			if notices, _ := f.expiryNoticeState(grant); notices != 1 {
				t.Fatalf("got %d notices, want 1", notices)
			}
		})
	}
}

func TestCreditExpiryNotificationSuppressesAfterDirectoryLookup(t *testing.T) {
	t.Parallel()
	f, acct, _, id := expiryAmountFixture(t)
	first, second := f.resendRecipients(acct)
	lookup, release := make(chan struct{}), make(chan struct{})
	f.s.directory = notificationDirectoryFunc(func(ctx context.Context, userID string) (directory.User, error) {
		if userID == second {
			close(lookup)
			select {
			case <-release:
			case <-ctx.Done():
				return directory.User{}, ctx.Err()
			}
		}
		return f.dir.User(ctx, userID)
	})
	deliveryCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.s.DeliverNotifications(deliveryCtx) }()
	select {
	case <-lookup:
	case err := <-done:
		t.Fatalf("delivery ended before second lookup: %v", err)
	case <-deliveryCtx.Done():
		t.Fatal(deliveryCtx.Err())
	}
	f.deduct(acct, 10*usd)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st := f.noticeState(id)
	if !st.Abandoned || st.Sent || !slices.Equal(st.Recipients, []string{first}) || len(f.email.Messages()) != 1 {
		t.Fatalf("spent during second lookup: state %+v, got %d sends", st, len(f.email.Messages()))
	}
}

func TestCreditExpiryNotificationKeepsAttemptedAmount(t *testing.T) {
	t.Parallel()
	for _, spend := range []int64{4 * usd, 10 * usd} {
		t.Run(FormatUSD(spend), func(t *testing.T) {
			f, acct, grant, id := expiryAmountFixture(t)
			first, second := f.resendRecipients(acct)
			firstKey, secondKey := creditExpiryNotificationPrefix+grant+"/"+first, creditExpiryNotificationPrefix+grant+"/"+second
			f.s.directory = notificationDirectoryFunc(func(ctx context.Context, userID string) (directory.User, error) {
				if userID == second {
					return directory.User{}, errors.New("directory unavailable")
				}
				return f.dir.User(ctx, userID)
			})
			provider := &notificationProvider{}
			var acceptedFirst atomic.Bool
			logs := f.useNotificationProvider(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if acceptedFirst.CompareAndSwap(false, true) {
					var amount int64
					err := f.db.Pool.QueryRow(r.Context(), `SELECT amount_usd_micros FROM billing_notification_expiry_amounts
						WHERE notification_id = $1 AND user_id = $2`, id, first).Scan(&amount)
					if err != nil || amount != 10*usd {
						t.Errorf("provider call before amount committed: amount %d, error %v", amount, err)
					}
					provider.ServeHTTP(httptest.NewRecorder(), r)
					// Resend accepted this request but the worker did not obtain its ID.
					_, _ = w.Write([]byte("null"))
					return
				}
				provider.ServeHTTP(w, r)
			}))
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			provider.assertKeys(t, firstKey)
			if st := f.noticeState(id); st.Sent || st.Abandoned || st.Attempts != 1 || len(st.Recipients) != 0 {
				t.Fatalf("unknown outcome state %+v", st)
			}
			f.deduct(acct, spend)
			f.s.directory = f.dir
			f.clock.Advance(time.Minute)
			if n := f.count(`SELECT count(*) FROM billing_notifications
				WHERE send_time IS NULL AND abandon_time IS NULL AND next_attempt_time <= $1`, f.clock.Now()); n != 0 {
				t.Fatal("previous worker could claim the notice")
			}
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			st := f.noticeState(id)
			if spend == 10*usd {
				provider.assertKeys(t, firstKey)
				if !st.Abandoned || st.Sent || len(st.Recipients) != 0 {
					t.Fatalf("spent before retry: %+v", st)
				}
				return
			}
			provider.assertKeys(t, firstKey, firstKey, secondKey)
			if !st.Sent || st.Abandoned || !slices.Equal(st.Recipients, []string{first, second}) ||
				strings.Contains(logs.String(), "idempotency_payload_conflict") {
				t.Fatalf("retry did not replay the original body and deliver to both recipients: %+v", st)
			}
			provider.mu.Lock()
			defer provider.mu.Unlock()
			if !strings.Contains(string(provider.accepted[firstKey].payload), "$10.00 of credit expires") ||
				!strings.Contains(string(provider.accepted[secondKey].payload), "$6.00 of credit expires") {
				t.Fatal("accepted amounts did not match the balance at the first attempt for each key")
			}
		})
	}
}

func TestCreditExpiryNotificationLegacyAmount(t *testing.T) {
	t.Parallel()
	for _, spend := range []int64{4 * usd, 10 * usd} {
		t.Run(FormatUSD(spend), func(t *testing.T) {
			f, acct, _, id := expiryAmountFixture(t)
			// Recreate the old row format. A worker could have sent from
			// this row with attempts still zero and without a receipt.
			f.execResendSQL(`UPDATE billing_notifications SET next_attempt_time = expiry_next_attempt_time,
				expiry_next_attempt_time = NULL WHERE notification_id = $1`, id)
			f.deduct(acct, spend)
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			st, msgs := f.noticeState(id), f.email.Messages()
			if spend == 10*usd {
				if !st.Abandoned || st.Sent || len(msgs) != 0 {
					t.Fatalf("spent legacy grant: state %+v, got %d sends", st, len(msgs))
				}
				return
			}
			if !st.Sent || st.Abandoned || len(msgs) != 2 {
				t.Fatalf("usable legacy grant: state %+v, got %d sends", st, len(msgs))
			}
			for _, msg := range msgs {
				if !strings.Contains(msg.Text, "$10.00 of credit expires") {
					t.Fatalf("legacy email changed its queued amount: %s", msg.Text)
				}
			}
			if n := f.count(`SELECT count(*) FROM billing_notification_expiry_amounts WHERE notification_id = $1`, id); n != 0 {
				t.Fatal("legacy notice should use its queued amount")
			}
		})
	}
}

func TestCreditExpiryNotificationChecksGrantIdentityAndExpiry(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"absent grant", "different account", "malformed key", "grant expired"} {
		t.Run(name, func(t *testing.T) {
			f, _, grant, id := expiryAmountFixture(t)
			switch name {
			case "absent grant":
				f.execResendSQL(`UPDATE billing_notifications SET dedup_key = $2 WHERE notification_id = $1`,
					id, creditExpiryNotificationPrefix+"01900000-0000-7000-8000-000000000001")
			case "different account":
				f.execResendSQL(`UPDATE billing_notifications SET billing_account_id = $2 WHERE notification_id = $1`, id, f.account())
			case "malformed key":
				f.execResendSQL(`UPDATE billing_notifications SET dedup_key = 'credit_expiry/not-a-grant' WHERE notification_id = $1`, id)
			case "grant expired":
				f.execResendSQL(`UPDATE credit_grants SET expire_time = $2 WHERE credit_grant_id = $1`, grant, f.clock.Now())
			}
			if err := f.s.DeliverNotifications(ctx()); err != nil {
				t.Fatal(err)
			}
			if st := f.noticeState(id); !st.Abandoned || st.Sent || len(f.email.Messages()) != 0 {
				t.Fatalf("expected suppression, state %+v, got %d sends", st, len(f.email.Messages()))
			}
		})
	}
}

func TestCreditExpiryNotificationRetriesGrantReadFailure(t *testing.T) {
	t.Parallel()
	f, _, _, id := expiryAmountFixture(t)
	f.execResendSQL("ALTER TABLE credit_grants RENAME COLUMN remaining_usd_micros TO remaining_unavailable")
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(id); st.Sent || st.Abandoned || st.Attempts != 1 ||
		!st.Next.Equal(f.clock.Now().Add(time.Minute)) || len(f.email.Messages()) != 0 {
		t.Fatalf("grant read failure did not schedule an ordinary retry: %+v", st)
	}
	f.execResendSQL("ALTER TABLE credit_grants RENAME COLUMN remaining_unavailable TO remaining_usd_micros")
	f.clock.Advance(time.Minute)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(id); !st.Sent || st.Abandoned || st.Attempts != 2 || len(f.email.Messages()) != 2 {
		t.Fatalf("grant read recovery did not deliver the original notice: %+v", st)
	}
}
