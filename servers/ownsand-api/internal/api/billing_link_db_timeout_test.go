// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestBillingLinkOwnershipQueryTimesOut(t *testing.T) {
	t.Parallel()
	const lookupBudget = time.Second
	lookup := make(chan context.Context, 1)
	startQuery := make(chan struct{})
	var f *linkAttemptFixture
	f = newLinkAttemptFixture(t, "stalled-lookup", func(o *app.Options) {
		o.DB = linkDispatchDB(t, o.DB, linkDispatchTrace{
			insert: func() { f.env.Clock.Advance(billing.LinkCallTimeout - lookupBudget) },
			lookupStart: func(ctx context.Context) {
				lookup <- ctx
				select {
				case <-startQuery:
				case <-t.Context().Done():
				}
			},
			lookupEnd: func() {},
		})
	})

	done := make(chan error, 1)
	go func() { done <- f.linkWithContext(t.Context(), f.org.OwnerClient, billingAccountA) }()
	var lookupCtx context.Context
	select {
	case lookupCtx = <-lookup:
	case err := <-done:
		t.Fatalf("link returned before the ownership lookup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("ownership lookup did not start")
	}
	deadline, ok := lookupCtx.Deadline()
	if !ok || time.Until(deadline) > lookupBudget {
		t.Fatalf("ownership lookup deadline = %v, want at most %v remaining", deadline, lookupBudget)
	}
	before := f.pendingLinkAttempt(t)
	tx := f.holdLinkDBLock(t, "LOCK TABLE billing_pending_unlinks IN ACCESS EXCLUSIVE MODE")
	if err := lookupCtx.Err(); err != nil {
		t.Fatalf("ownership lookup ran out of time before the lock was in place: %v", err)
	}
	close(startQuery)

	// The table lock blocks both the SELECT and the fallback DELETE. Keep it
	// held through the return to exercise the lookup and cleanup deadlines.
	started := time.Now()
	f.wantLinkResult(t, done, connect.CodeUnavailable, lookupBudget+20*time.Second)
	t.Logf("ownership lookup and abandon returned after %v", time.Since(started))
	if !errors.Is(lookupCtx.Err(), context.DeadlineExceeded) {
		t.Fatalf("ownership lookup context ended with %v, want deadline exceeded", lookupCtx.Err())
	}
	for _, event := range []string{"billing_link_attempt_expired_before_dispatch", "billing_link_attempt_abandon_failed"} {
		if !strings.Contains(f.env.Logs.String(), event) {
			t.Errorf("missing log event %s", event)
		}
	}
	if got := f.fake.Calls(accountstest.MethodLinkOrganization); got != 0 {
		t.Errorf("Accounts link calls = %d, want zero", got)
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.checkPendingLinkRecovery(t, before, false)
}

func TestBillingLinkCleanupTimesOut(t *testing.T) {
	for _, tc := range []struct {
		name, lock, failedEvent string
		reply                   connect.Code
		rejected                bool
		wantWait                time.Duration
	}{
		{
			name: "abandon after rejection", rejected: true, reply: connect.CodePermissionDenied, wantWait: 15 * time.Second,
			lock:        "SELECT 1 FROM organizations WHERE organization_id = $1 FOR NO KEY UPDATE",
			failedEvent: "billing_link_attempt_abandon_failed",
		},
		{
			name: "release after completion timeout", reply: connect.CodeDeadlineExceeded, wantWait: 30 * time.Second,
			lock:        "SELECT 1 FROM billing_pending_unlinks WHERE organization_id = $1 FOR UPDATE",
			failedEvent: "billing_link_attempt_release_failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newLinkAttemptFixture(t, "stalled-cleanup")
			if tc.rejected {
				f.fake.Fail(accountstest.MethodLinkOrganization, connect.NewError(connect.CodePermissionDenied, errors.New("billing account rejected")))
			}
			gate := f.fake.Hold(accountstest.MethodLinkOrganization)
			t.Cleanup(gate.Release)
			done := make(chan error, 1)
			go func() { done <- f.linkWithContext(t.Context(), f.org.OwnerClient, billingAccountA) }()
			f.waitForAccountsLink(t, gate, done)
			before := f.pendingLinkAttempt(t)
			tx := f.holdLinkDBLock(t, tc.lock, f.org.ID)

			started := time.Now()
			gate.Release()
			f.wantLinkResult(t, done, tc.reply, tc.wantWait+5*time.Second)
			elapsed := time.Since(started)
			t.Logf("link returned %v after %v with the lock held", tc.reply, elapsed)
			if elapsed < tc.wantWait {
				t.Errorf("link returned before spending %v on the blocked database operations", tc.wantWait)
			}
			if !strings.Contains(f.env.Logs.String(), tc.failedEvent) {
				t.Errorf("missing log event %s", tc.failedEvent)
			}
			if err := tx.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			f.checkPendingLinkRecovery(t, before, !tc.rejected)
		})
	}
}

func TestBillingLinkCompletesAfterCallerCancellation(t *testing.T) {
	f := newLinkAttemptFixture(t, "canceled-after-commit")
	gate := f.fake.Hold(accountstest.MethodLinkOrganization)
	t.Cleanup(gate.Release)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- f.linkWithContext(ctx, f.org.OwnerClient, billingAccountA) }()
	f.waitForAccountsLink(t, gate, done)
	f.wantRows(t, 1, 1)
	cancel()
	f.wantLinkResult(t, done, connect.CodeCanceled, 5*time.Second)
	gate.Release()

	testenv.Eventually(t, "committed link and removed attempt", func() bool {
		var complete bool
		err := f.env.DB.Pool.QueryRow(t.Context(), `SELECT b.billing_account_id IS NOT DISTINCT FROM $2::uuid
			AND NOT EXISTS (SELECT 1 FROM billing_pending_unlinks p WHERE p.organization_id = b.organization_id)
			FROM organization_billing b WHERE b.organization_id = $1`, f.org.ID, billingAccountA).Scan(&complete)
		return err == nil && complete
	})
	f.wantRemoteLink(t, true)
	if got := f.fake.Calls(accountstest.MethodLinkOrganization); got != 1 {
		t.Errorf("Accounts link calls = %d, want one", got)
	}
}

func (f *linkAttemptFixture) holdLinkDBLock(t *testing.T, query string, args ...any) pgx.Tx {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	tx, err := f.env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tx.Rollback(ctx)
	})
	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatal(err)
	}
	return tx
}

func (f *linkAttemptFixture) waitForAccountsLink(t *testing.T, gate *accountstest.Gate, done <-chan error) {
	t.Helper()
	select {
	case <-gate.Entered:
	case err := <-done:
		t.Fatalf("link returned before reaching Accounts: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Accounts did not receive the link")
	}
}

func (f *linkAttemptFixture) wantLinkResult(t *testing.T, done <-chan error, code connect.Code, timeout time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		if actual := testenv.Code(err); actual != code {
			t.Fatalf("link returned %v, want %v", err, code)
		}
	case <-time.After(timeout):
		t.Fatalf("link did not return within %v", timeout)
	}
}

type pendingLinkAttempt struct {
	id                  string
	deadline, nextRetry time.Time
	attempts            int
}

func (f *linkAttemptFixture) pendingLinkAttempt(t *testing.T) pendingLinkAttempt {
	t.Helper()
	var row pendingLinkAttempt
	err := f.env.DB.Pool.QueryRow(t.Context(), `SELECT link_attempt_id::text, link_call_deadline, next_attempt_time, attempts
		FROM billing_pending_unlinks WHERE organization_id = $1`, f.org.ID).Scan(&row.id, &row.deadline, &row.nextRetry, &row.attempts)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func (f *linkAttemptFixture) checkPendingLinkRecovery(t *testing.T, want pendingLinkAttempt, linkedRemotely bool) {
	t.Helper()
	f.wantRemoteLink(t, linkedRemotely)
	f.wantLocalLink(t, false)
	restarted := f.restart()
	if err := restarted.RetryPendingUnlinks(t.Context()); err != nil {
		t.Fatal(err)
	}
	got := f.pendingLinkAttempt(t)
	if got.id != want.id || !got.deadline.Equal(want.deadline) || !got.nextRetry.Equal(want.nextRetry) || got.attempts != want.attempts {
		t.Fatalf("pending attempt after timeout and early reconciliation = %+v, want %+v", got, want)
	}
	f.env.Clock.Advance(want.deadline.Sub(f.env.Clock.Now()))
	if err := restarted.RetryPendingUnlinks(t.Context()); err != nil {
		t.Fatal(err)
	}
	f.wantRows(t, 0, 0)
	f.wantRemoteLink(t, false)
	f.wantLocalLink(t, false)
}
