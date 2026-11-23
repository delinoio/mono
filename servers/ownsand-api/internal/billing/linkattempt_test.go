// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

// TestLinkCallBudget checks the dispatch admission rule: a call is admitted
// only when it, and the time that delino-accounts can still need after the
// client gave up, end before the persisted call deadline.
func TestLinkCallBudget(t *testing.T) {
	t0 := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	deadline := t0.Add(billing.LinkCallTimeout + 15*time.Second)
	for _, tc := range []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{"prompt", t0, billing.LinkCallTimeout},
		{"slightly late", t0.Add(10 * time.Second), 5 * time.Second},
		{"exactly out of budget", t0.Add(billing.LinkCallTimeout), 0},
		{"expired", t0.Add(time.Minute), -45 * time.Second},
		{"before the persisted time", t0.Add(-time.Hour), billing.LinkCallTimeout},
	} {
		if got := billing.LinkCallBudget(deadline, tc.now); got != tc.want {
			t.Errorf("%s: budget = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestLinkAttemptRunning checks that only an attempt that still owns its row
// with a running call is admitted for dispatch: an ownership transfer, a
// release of the call and a missing row all end it.
func TestLinkAttemptRunning(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	const orgID, attempt = "0190f3a4-0000-7000-8000-0000000000d1", "0190f3a4-0000-7000-8000-0000000000e1"
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	running := func() bool {
		t.Helper()
		ok, err := billing.LinkAttemptRunning(ctx, d.Pool, orgID, attempt)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if running() {
		t.Fatal("an attempt without a row is running")
	}
	var deadline time.Time
	if err := d.InTx(ctx, func(tx pgx.Tx) (err error) {
		deadline, err = billing.BeginLinkAttemptTx(ctx, tx, orgID, attempt, accountA, now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(billing.LinkCallTimeout + 15*time.Second); !deadline.Equal(want) {
		t.Fatalf("deadline = %v, want %v", deadline, want)
	}
	if !running() {
		t.Fatal("a recorded attempt is not running")
	}
	if ok, err := billing.LinkAttemptRunning(ctx, d.Pool, orgID, "0190f3a4-0000-7000-8000-0000000000e2"); err != nil || ok {
		t.Fatalf("another attempt ID is running: %v, %v", ok, err)
	}
	var taken bool
	if err := d.InTx(ctx, func(tx pgx.Tx) (err error) {
		taken, err = billing.TakeOverLinkAttemptTx(ctx, tx, orgID, now.Add(time.Second))
		return err
	}); err != nil || !taken {
		t.Fatalf("takeover = %v, %v", taken, err)
	}
	if running() {
		t.Fatal("a taken-over attempt is running")
	}
	var guard *time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT unlink_not_before FROM billing_pending_unlinks WHERE organization_id = $1", orgID).Scan(&guard); err != nil {
		t.Fatal(err)
	}
	if guard == nil || !guard.Equal(deadline) {
		t.Fatalf("unlink_not_before = %v, want the call deadline %v", guard, deadline)
	}
}
