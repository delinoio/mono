// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
)

// TestPendingUnlinkBackoffStartsAtFailure checks that the back-off of a
// failed pending unlink starts when the delino-accounts call failed, not when
// the attempt began: the time that the call takes does not consume the delay.
func TestPendingUnlinkBackoffStartsAtFailure(t *testing.T) {
	t0 := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.addPendingUnlink(t, org1)
	f.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))

	// attempt 1: the call takes 2 seconds, the delay is 1 second.
	gate := f.fake.Hold(accountstest.MethodUnlinkOrganization)
	t.Cleanup(gate.Release)
	done := make(chan error, 1)
	go func() { done <- f.statuses.UnlinkPending(ctx, org1) }()
	<-gate.Entered
	f.clock.Advance(2 * time.Second)
	gate.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a, next, ok := f.pendingUnlink(t, org1); !ok || a != 1 || !next.Equal(t0.Add(3*time.Second)) {
		t.Fatalf("after a failure at t0+2s: attempts=%d next=%v ok=%v, want 1 and t0+3s", a, next, ok)
	}

	// attempt 2 (due at t0+3s): the call takes 5 seconds, the delay is 2 seconds.
	f.clock.Advance(time.Second)
	gate = f.fake.Hold(accountstest.MethodUnlinkOrganization)
	t.Cleanup(gate.Release)
	go func() { done <- f.statuses.UnlinkPending(ctx, org1) }()
	<-gate.Entered
	f.clock.Advance(5 * time.Second)
	gate.Release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a, next, ok := f.pendingUnlink(t, org1); !ok || a != 2 || !next.Equal(t0.Add(10*time.Second)) {
		t.Fatalf("after a failure at t0+8s: attempts=%d next=%v ok=%v, want 2 and t0+10s", a, next, ok)
	}
}

// TestPendingUnlinkBackoffAfterLockWait checks that the wait for the
// organization lock does not consume the back-off either.
func TestPendingUnlinkBackoffAfterLockWait(t *testing.T) {
	t0 := time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.addPendingUnlink(t, org1)
	f.fake.Fail(accountstest.MethodUnlinkOrganization, connect.NewError(connect.CodeUnavailable, errors.New("down")))

	holder, err := f.db.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", org1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.statuses.UnlinkPending(ctx, org1) }()
	// Wait until the attempt blocks on the organization row.
	deadline := time.Now().Add(10 * time.Second)
	for {
		var waiting int
		if err := f.db.Pool.QueryRow(ctx, "SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%FOR NO KEY UPDATE%'").Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the attempt did not wait for the organization lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	f.clock.Advance(10 * time.Second)
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a, next, ok := f.pendingUnlink(t, org1); !ok || a != 1 || !next.Equal(t0.Add(11*time.Second)) {
		t.Fatalf("after a failure at t0+10s: attempts=%d next=%v ok=%v, want 1 and t0+11s", a, next, ok)
	}
}

// TestPendingUnlinkGuardAndSuccess checks that a worker one moment before
// the guard of a pending unlink sends nothing and keeps the row untouched,
// and that a worker at the guard sends the unlink and deletes the row.
func TestPendingUnlinkGuardAndSuccess(t *testing.T) {
	t0 := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.addPendingUnlink(t, org1)
	guard := t0.Add(30 * time.Second)
	if _, err := f.db.Pool.Exec(ctx, "UPDATE billing_pending_unlinks SET unlink_not_before = $2, next_attempt_time = $2 WHERE organization_id = $1", org1, guard); err != nil {
		t.Fatal(err)
	}

	f.clock.Set(guard.Add(-time.Millisecond))
	if err := f.statuses.UnlinkPending(ctx, org1); err != nil {
		t.Fatal(err)
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 0 {
		t.Fatalf("UnlinkOrganization calls = %d before the guard, want 0", n)
	}
	if a, next, ok := f.pendingUnlink(t, org1); !ok || a != 0 || !next.Equal(guard) {
		t.Fatalf("before the guard: attempts=%d next=%v ok=%v, want the row untouched", a, next, ok)
	}

	f.clock.Set(guard)
	if err := f.statuses.UnlinkPending(ctx, org1); err != nil {
		t.Fatal(err)
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 1 {
		t.Fatalf("UnlinkOrganization calls = %d at the guard, want 1", n)
	}
	if _, _, ok := f.pendingUnlink(t, org1); ok {
		t.Fatal("the pending unlink stayed after a success")
	}
	if _, _, linked := f.fake.Link(org1); linked {
		t.Fatal("delino-accounts still has the link")
	}
}
