// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
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

// queueNotice queues a $5 credit expiry notice and returns its ID and key.
func (f *fixture) queueNotice(acct string) (id, key string) {
	f.t.Helper()
	grantID, id := f.queueExpiryNotice(acct, 5*usd, f.clock.Now().Add(24*time.Hour))
	return id, creditExpiryNotificationPrefix + grantID
}

func (f *fixture) queueExpiryNotice(acct string, amount int64, expiry time.Time) (grantID, noticeID string) {
	f.t.Helper()
	grantID = f.grant(acct, LedgerSubscriptionCredit, amount)
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE credit_grants SET expire_time = $2 WHERE credit_grant_id = $1`, grantID, expiry); err != nil {
		f.t.Fatal(err)
	}
	if err := f.s.QueueExpiryNotices(ctx()); err != nil {
		f.t.Fatal(err)
	}
	if err := f.db.Pool.QueryRow(ctx(), `SELECT notification_id::text FROM billing_notifications WHERE dedup_key = $1`,
		creditExpiryNotificationPrefix+grantID).Scan(&noticeID); err != nil {
		f.t.Fatal(err)
	}
	return grantID, noticeID
}

type noticeState struct {
	Attempts          int
	Sent              bool
	Abandoned         bool
	Next              time.Time
	Recipients        []string
	ResumeAfterUserID *string
}

func (f *fixture) noticeState(id string) noticeState {
	f.t.Helper()
	var st noticeState
	if err := f.db.Pool.QueryRow(ctx(), `SELECT attempts, send_time IS NOT NULL, abandon_time IS NOT NULL,
		COALESCE(expiry_next_attempt_time, next_attempt_time), resume_after_user_id
		FROM billing_notifications WHERE notification_id = $1`, id).Scan(&st.Attempts, &st.Sent, &st.Abandoned, &st.Next, &st.ResumeAfterUserID); err != nil {
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
	interrupted, key := f.queueNotice(acct)

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
	if st.Sent || st.Abandoned || st.Attempts != 0 || len(st.Recipients) != 1 || st.ResumeAfterUserID != nil {
		t.Fatalf("state after the deadline: %+v", st)
	}
	if want := f.clock.Now().Add(notificationDeferDelay); !st.Next.Equal(want) {
		t.Fatalf("next attempt %v, want %v", st.Next, want)
	}

	// A notification that is due now runs before the deferred one.
	f.s.email = f.email
	other := f.account()
	newer, _ := f.queueNotice(other)
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
	if userID == "" || got.IdempotencyKey != key+"/"+userID {
		t.Fatalf("resumed message %+v", got)
	}
}

func TestNotificationDeadlineResumesAfterLastStartedRecipient(t *testing.T) {
	for _, boundary := range []string{"pending", "removed", "recorded"} {
		t.Run(boundary, func(t *testing.T) {
			f := newFixture(t)
			acct := f.account()
			key := "low_balance/test"
			now := f.clock.Now()
			f.tx(func(tx pgx.Tx) error {
				if _, err := tx.Exec(t.Context(), `DELETE FROM billing_account_members WHERE billing_account_id = $1`, acct); err != nil {
					return err
				}
				if _, err := tx.Exec(t.Context(), `INSERT INTO billing_account_members (billing_account_id, user_id, role, create_time)
					VALUES ($1, 'a', 'owner', $2), ($1, 'b', 'admin', $2), ($1, 'c', 'admin', $2),
						($1, 'd', 'admin', $2), ($1, 'viewer', 'viewer', $2)`, acct, now); err != nil {
					return err
				}
				return f.s.enqueue(t.Context(), tx, acct, NotifyLowBalance, key, nil, now)
			})
			var id string
			if err := f.db.Pool.QueryRow(t.Context(), `SELECT notification_id::text FROM billing_notifications WHERE dedup_key = $1`, key).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if st := f.noticeState(id); st.ResumeAfterUserID != nil {
				t.Fatalf("new notice has a resume ID: %q", *st.ResumeAfterUserID)
			}

			var lookedUp []string
			f.s.notificationDeliveryBudget = time.Second
			f.s.directory = notificationDirectoryFunc(func(ctx context.Context, userID string) (directory.User, error) {
				lookedUp = append(lookedUp, userID)
				if userID == "c" {
					<-ctx.Done()
				}
				return directory.User{}, context.DeadlineExceeded
			})
			if err := f.s.deliverWorkerNotifications(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("delivery deadline: %v", err)
			}
			if !slices.Equal(lookedUp, []string{"a", "b", "c"}) {
				t.Fatalf("first pass looked up %v", lookedUp)
			}
			checkDeferred := func() {
				t.Helper()
				st := f.noticeState(id)
				if st.Sent || st.Abandoned || st.Attempts != 0 || len(st.Recipients) != 0 ||
					!st.Next.Equal(f.clock.Now().Add(notificationDeferDelay)) ||
					st.ResumeAfterUserID == nil || *st.ResumeAfterUserID != "c" {
					t.Fatalf("deferred notice = %+v, want resume ID c, one minute delay, no attempts or receipts", st)
				}
			}
			checkDeferred()

			// A claim that runs out of time before any lookup keeps the saved ID.
			f.clock.Advance(notificationDeferDelay)
			n, ok, err := f.s.claimNotification(t.Context())
			if err != nil || !ok {
				t.Fatalf("claim: %v %v", ok, err)
			}
			expired, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
			defer cancel()
			if err := f.s.deliver(expired, n); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("already expired pass: %v", err)
			}
			checkDeferred()

			// Parent shutdown during d's lookup keeps c as the starting boundary.
			f.clock.Advance(notificationDeferDelay)
			parent, stop := context.WithCancel(t.Context())
			defer stop()
			lookedUp = nil
			f.s.directory = notificationDirectoryFunc(func(ctx context.Context, userID string) (directory.User, error) {
				lookedUp = append(lookedUp, userID)
				stop()
				return directory.User{}, ctx.Err()
			})
			if err := f.s.deliverWorkerNotifications(parent); !errors.Is(err, context.Canceled) {
				t.Fatalf("canceled delivery: %v", err)
			}
			if !slices.Equal(lookedUp, []string{"d"}) {
				t.Fatalf("canceled pass looked up %v", lookedUp)
			}
			checkDeferred()

			wantOrder := []string{"d", "a", "b", "c"}
			switch boundary {
			case "removed":
				wantOrder = wantOrder[:3]
				if _, err := f.db.Pool.Exec(t.Context(), `DELETE FROM billing_account_members
					WHERE billing_account_id = $1 AND user_id = 'c'`, acct); err != nil {
					t.Fatal(err)
				}
			case "recorded":
				wantOrder = wantOrder[:3]
				if _, err := f.db.Pool.Exec(t.Context(), `INSERT INTO billing_notification_deliveries (notification_id, user_id, send_time)
					VALUES ($1, 'c', $2)`, id, f.clock.Now()); err != nil {
					t.Fatal(err)
				}
			}

			f.clock.Advance(notificationDeferDelay)
			worker := newFixtureOn(t, f.db)
			worker.clock.Set(f.clock.Now())
			lookedUp = nil
			worker.s.directory = notificationDirectoryFunc(func(_ context.Context, userID string) (directory.User, error) {
				lookedUp = append(lookedUp, userID)
				if userID == "d" {
					return directory.User{PrimaryEmail: "d@example.com"}, nil
				}
				return directory.User{}, errSendTest
			})
			if err := worker.s.deliverWorkerNotifications(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(lookedUp, wantOrder) {
				t.Fatalf("resumed pass looked up %v, want %v", lookedUp, wantOrder)
			}
			if msgs := worker.email.Messages(); len(msgs) != 1 || msgs[0].To != "d@example.com" || msgs[0].IdempotencyKey != key+"/d" {
				t.Fatalf("resumed pass sent %v, want one email to d with the original key", msgs)
			}
			wantRecipients := []string{"d"}
			if boundary == "recorded" {
				wantRecipients = []string{"c", "d"}
			}
			st := worker.noticeState(id)
			if st.Attempts != 1 || st.Sent || st.Abandoned || st.ResumeAfterUserID != nil ||
				!st.Next.Equal(worker.clock.Now().Add(time.Minute)) || !slices.Equal(st.Recipients, wantRecipients) {
				t.Fatalf("ordinary failure after resuming: %+v, want one attempt, one minute backoff, cleared resume ID and receipts %v", st, wantRecipients)
			}

			worker.clock.Advance(time.Minute)
			lookedUp = nil
			worker.s.directory = notificationDirectoryFunc(func(_ context.Context, userID string) (directory.User, error) {
				lookedUp = append(lookedUp, userID)
				return directory.User{PrimaryEmail: userID + "@example.com"}, nil
			})
			if err := worker.s.deliverWorkerNotifications(t.Context()); err != nil {
				t.Fatal(err)
			}
			st = worker.noticeState(id)
			if !slices.Equal(lookedUp, wantOrder[1:]) || !st.Sent || st.Attempts != 2 || len(st.Recipients) != len(wantOrder)+len(wantRecipients)-1 {
				t.Fatalf("completed retry looked up %v and saved %+v", lookedUp, st)
			}
		})
	}
}

// A pass that ends after the claim and before the first send defers the
// notification, too.
func TestNotificationCanceledPassDefers(t *testing.T) {
	f := newFixture(t)
	acct := f.account()
	id, _ := f.queueNotice(acct)
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
	id, _ := f.queueNotice(acct)

	workersCtx, cancelWorkers := context.WithTimeout(t.Context(), 15*time.Second)
	type secondWorkerKey struct{}
	secondCtx := context.WithValue(workersCtx, secondWorkerKey{}, true)
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseFirst := sync.OnceFunc(func() { close(release) })
	unexpectedSend := make(chan error, 1)
	var mu sync.Mutex
	firstSendStarted := false
	inFlight, maxInFlight := 0, 0
	f.s.email = &hookSender{next: f.email, before: func(ctx context.Context, _ email.Message) error {
		secondWorker := ctx.Value(secondWorkerKey{}) == true
		mu.Lock()
		inFlight++
		maxInFlight = max(maxInFlight, inFlight)
		firstSend := !secondWorker && !firstSendStarted
		if firstSend {
			firstSendStarted = true
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			inFlight--
			mu.Unlock()
		}()
		if secondWorker {
			err := fmt.Errorf("second worker reached the sender while the first worker was held")
			select {
			case unexpectedSend <- err:
			default:
			}
			return err
		}
		if firstSend {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return ctx.Err()
	}}

	var firstResult, secondResult <-chan error
	t.Cleanup(func() {
		cancelWorkers()
		releaseFirst()
		// Notification settlement can continue after worker cancellation.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		join := func(name string, result <-chan error) {
			if result == nil {
				return
			}
			select {
			case <-result:
			case <-cleanupCtx.Done():
				t.Errorf("timed out waiting for the %s worker to exit during cleanup", name)
			}
		}
		join("first", firstResult)
		join("second", secondResult)
	})
	startWorker := func(ctx context.Context) <-chan error {
		result := make(chan error, 1)
		go func() { result <- f.s.DeliverNotifications(ctx) }()
		return result
	}

	firstResult = startWorker(workersCtx)
	select {
	case <-entered:
	case err := <-firstResult:
		firstResult = nil
		if err != nil {
			t.Fatalf("first worker exited before reaching the sender: %v", err)
		}
		t.Fatal("first worker exited before reaching the sender")
	case <-workersCtx.Done():
		t.Fatal("timed out waiting for the first worker to reach the sender")
	}

	// The second worker runs while the first one is blocked in a send.
	secondResult = startWorker(secondCtx)
	select {
	case err := <-unexpectedSend:
		t.Fatal(err)
	case err := <-secondResult:
		secondResult = nil
		// The send signal and the result can both be ready.
		select {
		case sendErr := <-unexpectedSend:
			t.Fatal(sendErr)
		default:
		}
		if err != nil {
			t.Fatalf("second worker failed: %v", err)
		}
		if workersCtx.Err() != nil {
			t.Fatal("worker deadline expired while the first sender was held")
		}
	case <-workersCtx.Done():
		t.Fatal("timed out waiting for the second worker to exit")
	}
	if got := len(f.email.Messages()); got != 0 {
		t.Fatalf("the second worker sent %d emails for a leased notification", got)
	}
	releaseFirst()
	select {
	case err := <-firstResult:
		firstResult = nil
		if err != nil {
			t.Fatalf("first worker failed: %v", err)
		}
	case <-workersCtx.Done():
		t.Fatal("timed out waiting for the first worker to exit after release")
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
	id, _ := f.queueNotice(acct)
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
	id, key := f.queueNotice(acct)
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
	if want := key + "/" + st.Recipients[0]; msgs[0].IdempotencyKey != want {
		t.Fatalf("key %q, want %q", msgs[0].IdempotencyKey, want)
	}
}
