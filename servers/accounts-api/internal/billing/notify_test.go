// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

// hookSender runs before delegates to the fake and lets a test stop, fail or
// observe a send.
type hookSender struct {
	next   email.Sender
	before func(ctx context.Context, msg email.Message) error
}

func (h *hookSender) Send(ctx context.Context, msg email.Message) error {
	if h.before != nil {
		if err := h.before(ctx, msg); err != nil {
			return err
		}
	}
	return h.next.Send(ctx, msg)
}

// queueNotice queues one credit expiry notification for the account and
// returns its ID.
func (f *fixture) queueNotice(acct, key string) string {
	f.t.Helper()
	err := f.db.InTx(ctx(), func(tx pgx.Tx) error {
		return f.s.enqueue(ctx(), tx, acct, NotifyCreditExpiry, key, map[string]string{
			notifyDetailAmount:     "5000000",
			notifyDetailExpireTime: f.clock.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339Nano),
		}, f.clock.Now())
	})
	if err != nil {
		f.t.Fatal(err)
	}
	var id string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT notification_id::text FROM billing_notifications WHERE dedup_key = $1`, key).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

type noticeState struct {
	Attempts   int
	Sent       bool
	Abandoned  bool
	Next       time.Time
	Recipients []string
}

func (f *fixture) noticeState(id string) noticeState {
	f.t.Helper()
	var st noticeState
	if err := f.db.Pool.QueryRow(ctx(), `SELECT attempts, send_time IS NOT NULL, abandon_time IS NOT NULL, next_attempt_time
		FROM billing_notifications WHERE notification_id = $1`, id).Scan(&st.Attempts, &st.Sent, &st.Abandoned, &st.Next); err != nil {
		f.t.Fatal(err)
	}
	rows, err := f.db.Pool.Query(ctx(), `SELECT user_id FROM billing_notification_deliveries WHERE notification_id = $1 ORDER BY user_id`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	if st.Recipients, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		f.t.Fatal(err)
	}
	return st
}

// An expired pass deadline keeps the recipient that already received the
// email, defers the notification without counting an attempt, lets a newer
// due notification run first and resumes with the unfinished recipient only.
func TestNotificationDeadlineKeepsDeliveryProgress(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	interrupted := f.queueNotice(acct, "credit_expiry/deadline-a")

	passCtx, cancel := context.WithTimeout(ctx(), 300*time.Millisecond)
	defer cancel()
	sends := 0
	f.s.email = &hookSender{next: f.email, before: func(ctx context.Context, _ email.Message) error {
		sends++
		if sends == 2 {
			// The second send hangs until the pass deadline expires.
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	}}
	if err := f.s.DeliverNotifications(passCtx); err == nil {
		t.Fatal("an expired pass reported no error")
	}
	first := f.email.Messages()
	if len(first) != 1 {
		t.Fatalf("emails before the deadline: %d, want 1", len(first))
	}
	st := f.noticeState(interrupted)
	if st.Sent || st.Abandoned || st.Attempts != 0 || len(st.Recipients) != 1 {
		t.Fatalf("state after the deadline: %+v", st)
	}
	if want := f.clock.Now().Add(notificationDeferDelay); !st.Next.Equal(want) {
		t.Fatalf("next attempt %v, want %v", st.Next, want)
	}

	// A notification that is due now runs before the deferred one.
	f.s.email = f.email
	other := f.account()
	newer := f.queueNotice(other, "credit_expiry/deadline-b")
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(newer); !st.Sent || len(st.Recipients) != 2 {
		t.Fatalf("newer notification: %+v", st)
	}
	if st := f.noticeState(interrupted); st.Sent || len(st.Recipients) != 1 {
		t.Fatalf("deferred notification ran early: %+v", st)
	}
	if got := len(f.email.Messages()); got != 3 {
		t.Fatalf("emails: %d, want 3", got)
	}

	// After the delay, only the unfinished recipient gets the email, with the
	// original idempotency key.
	f.clock.Advance(notificationDeferDelay)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	st = f.noticeState(interrupted)
	if !st.Sent || st.Attempts != 1 || len(st.Recipients) != 2 {
		t.Fatalf("state after the resume: %+v", st)
	}
	msgs := f.email.Messages()
	if len(msgs) != 4 {
		t.Fatalf("emails: %d, want 4", len(msgs))
	}
	got := msgs[3]
	if got.To == first[0].To {
		t.Fatalf("the first recipient received the email twice: %s", got.To)
	}
	var userID string
	for _, id := range st.Recipients {
		if got.To == id+"@example.com" {
			userID = id
		}
	}
	if userID == "" || got.IdempotencyKey != "credit_expiry/deadline-a/"+userID {
		t.Fatalf("resumed message %+v", got)
	}
}

// A pass that ends after the claim and before the first send defers the
// notification, too.
func TestNotificationCanceledPassDefers(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	id := f.queueNotice(acct, "credit_expiry/canceled")
	n, ok, err := f.s.claimNotification(ctx())
	if err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	passCtx, cancel := context.WithCancel(ctx())
	cancel()
	if err := f.s.deliver(passCtx, n); err == nil {
		t.Fatal("a canceled pass reported no error")
	}
	if got := len(f.email.Messages()); got != 0 {
		t.Fatalf("a canceled pass sent %d emails", got)
	}
	st := f.noticeState(id)
	if st.Attempts != 0 || st.Sent || len(st.Recipients) != 0 || !st.Next.Equal(f.clock.Now().Add(notificationDeferDelay)) {
		t.Fatalf("state: %+v", st)
	}
}

// Two workers never process one notification at the same time: while the
// first worker is in a send, the second finds nothing due.
func TestNotificationConcurrentWorkersSkipLeased(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	id := f.queueNotice(acct, "credit_expiry/concurrent")

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	var mu sync.Mutex
	inFlight, maxInFlight := 0, 0
	f.s.email = &hookSender{next: f.email, before: func(ctx context.Context, _ email.Message) error {
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		once.Do(func() {
			close(entered)
			<-release
		})
		return nil
	}}

	done := make(chan error, 1)
	go func() { done <- f.s.DeliverNotifications(ctx()) }()
	<-entered
	// The second worker runs while the first one is blocked in a send.
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := len(f.email.Messages()); got != 0 {
		t.Fatalf("the second worker sent %d emails for a leased notification", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st := f.noticeState(id)
	if !st.Sent || st.Attempts != 1 || len(st.Recipients) != 2 {
		t.Fatalf("state: %+v", st)
	}
	if got := len(f.email.Messages()); got != 2 {
		t.Fatalf("emails: %d, want 2", got)
	}
	if maxInFlight != 1 {
		t.Fatalf("%d concurrent sends", maxInFlight)
	}
}

// A worker that died after the claim releases the notification when the lease
// ends, and the retry skips the recipients that are recorded.
func TestNotificationLeaseExpiryRetries(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	id := f.queueNotice(acct, "credit_expiry/lease")
	if _, ok, err := f.s.claimNotification(ctx()); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	if err := f.s.DeliverNotifications(ctx()); err != nil || len(f.email.Messages()) != 0 {
		t.Fatalf("delivered a leased notification: %v", err)
	}
	f.clock.Advance(notificationLease)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	if st := f.noticeState(id); !st.Sent || len(f.email.Messages()) != 2 {
		t.Fatalf("state after the lease: %+v, emails %d", st, len(f.email.Messages()))
	}
}

// An ordinary failure of the first recipient keeps the retry backoff and
// still sends to the later recipient with its key.
func TestNotificationOrdinaryFailureKeepsBackoff(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	id := f.queueNotice(acct, "credit_expiry/ordinary")
	var first string
	if err := f.db.Pool.QueryRow(ctx(), `SELECT min(user_id) FROM billing_account_members
		WHERE billing_account_id = $1 AND role IN ('owner', 'admin')`, acct).Scan(&first); err != nil {
		t.Fatal(err)
	}
	f.email.SetErrFor(first+"@example.com", errSendTest)
	if err := f.s.DeliverNotifications(ctx()); err != nil {
		t.Fatal(err)
	}
	st := f.noticeState(id)
	if st.Sent || st.Attempts != 1 || len(st.Recipients) != 1 {
		t.Fatalf("state: %+v", st)
	}
	if want := f.clock.Now().Add(time.Minute); !st.Next.Equal(want) {
		t.Fatalf("next attempt %v, want %v", st.Next, want)
	}
	msgs := f.email.Messages()
	if len(msgs) != 1 || msgs[0].IdempotencyKey == "" || msgs[0].To == first+"@example.com" {
		t.Fatalf("emails %+v", msgs)
	}
	if want := fmt.Sprintf("credit_expiry/ordinary/%s", st.Recipients[0]); msgs[0].IdempotencyKey != want {
		t.Fatalf("key %q, want %q", msgs[0].IdempotencyKey, want)
	}
}
