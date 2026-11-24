// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// Month boundary of the tests: the top-up is queued before and claimed after
// the end of the UTC month.
var (
	beforeMonthEnd = time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC)
	afterMonthEnd  = time.Date(2027, 1, 1, 0, 1, 0, 0, time.UTC)
)

// capFixture returns a fixture with an account that has an active
// subscription, a 30 USD balance and automatic top-ups of 25 USD under a cap
// of 25 USD, so one top-up uses the whole monthly cap.
func capFixture(t *testing.T) (*fixture, string) {
	t.Helper()
	f := newFixture(t)
	f.clock.Set(beforeMonthEnd)
	acct := f.account()
	f.activate(acct)
	f.grant(acct, LedgerManualTopUp, 30*usd)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 25*usd)
	return f, acct
}

func (f *fixture) monthToDate(acct string, at time.Time) int64 {
	f.t.Helper()
	mtd, err := MonthToDate(ctx(), f.db.Pool, acct, at)
	if err != nil {
		f.t.Fatal(err)
	}
	return mtd
}

const (
	lockCapAccount = `SELECT pg_backend_pid() FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`
	lockCapTopUp   = `SELECT pg_backend_pid() FROM top_ups WHERE billing_account_id = $1 AND status = 'pending' FOR UPDATE`
)

func (f *fixture) claimAfterMonthRollover(acct, lockQuery string) {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(ctx(), 15*time.Second)
	defer cancel()
	tx, err := f.db.Pool.Begin(ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	var holderPID int
	if err := tx.QueryRow(ctx, lockQuery, acct).Scan(&holderPID); err != nil {
		f.t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- f.s.ProcessAutoTopUps(ctx) }()

	// Advance the clock only after Postgres confirms the claim is blocked.
	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	for {
		var waiting bool
		if err := f.db.Pool.QueryRow(ctx, `SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity WHERE datname = current_database()
				AND $1 = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&waiting); err != nil {
			f.t.Fatalf("observe the claim lock wait: %v", err)
		}
		if waiting {
			break
		}
		select {
		case err := <-done:
			f.t.Fatalf("claim finished before waiting on the row lock: %v", err)
		case <-ctx.Done():
			f.t.Fatal("claim did not wait on the row lock")
		case <-poll.C:
		}
	}

	f.clock.Set(afterMonthEnd)
	if err := tx.Commit(ctx); err != nil {
		f.t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			f.t.Fatal(err)
		}
	case <-ctx.Done():
		f.t.Fatal("claim did not finish after releasing the row lock")
	}
}

func TestAutoTopUpClaimUsesMonthAfterLocks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		{"billing account", lockCapAccount},
		{"pending top-up", lockCapTopUp},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testAutoTopUpClaimUsesMonthAfterLock(t, tc.query)
		})
	}
}

func testAutoTopUpClaimUsesMonthAfterLock(t *testing.T, lockQuery string) {
	t.Helper()
	f, acct := capFixture(t)
	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	// Queued in December, the top-up already counts there.
	if got := f.monthToDate(acct, beforeMonthEnd); got != 25*usd {
		t.Fatalf("December month to date after queueing = %d", got)
	}

	f.claimAfterMonthRollover(acct, lockQuery)
	var createTime, capTime time.Time
	if err := f.db.Pool.QueryRow(ctx(), `SELECT create_time, cap_time FROM top_ups WHERE billing_account_id = $1`, acct).
		Scan(&createTime, &capTime); err != nil {
		t.Fatal(err)
	}
	if !createTime.Equal(beforeMonthEnd) || !capTime.Equal(afterMonthEnd) {
		t.Fatalf("create_time = %v, cap_time = %v, want %v and %v", createTime, capTime, beforeMonthEnd, afterMonthEnd)
	}
	charges := f.paddle.Charges()
	if len(charges) != 1 || paddletest.ChargeAmountCents(charges[0]) != "2500" {
		t.Fatalf("charges = %+v", charges)
	}
	f.completeCharge(charges[0], 2500)

	// Claimed in January, the top-up counts there and no longer in December.
	if got := f.monthToDate(acct, afterMonthEnd); got != 25*usd {
		t.Fatalf("January month to date = %d, want the claimed top-up", got)
	}
	if got := f.monthToDate(acct, beforeMonthEnd); got != 0 {
		t.Fatalf("December month to date = %d, want 0 after the claim moved the top-up", got)
	}
	// A later deduction sees the January use: the cap of 25 USD is spent.
	if res := f.deduct(acct, 25*usd); res.AutoTopUpQueued {
		t.Fatal("a top-up was queued although the claimed top-up used the whole January cap")
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "completed" {
		t.Fatalf("top-ups = %v", got)
	}
}

func TestAutoTopUpClaimedInTheQueueMonthCountsOnce(t *testing.T) {
	f, acct := capFixture(t)
	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	f.clock.Advance(30 * time.Second)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	f.completeCharge(f.paddle.Charges()[0], 2500)
	if got := f.monthToDate(acct, beforeMonthEnd); got != 25*usd {
		t.Fatalf("December month to date = %d, want one count", got)
	}
	if got := f.monthToDate(acct, afterMonthEnd); got != 0 {
		t.Fatalf("January month to date = %d, want 0", got)
	}
}

func TestFailedAutoTopUpReleasesTheCapOfTheClaimMonth(t *testing.T) {
	f, acct := capFixture(t)
	f.paddle.FailCharges(&paddletest.Failure{Status: http.StatusBadRequest, Code: paddle.CodePaymentDeclined})
	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	f.clock.Set(afterMonthEnd)
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if got := f.topUpStatuses(acct); len(got) != 1 || got[0] != "failed:payment_declined" {
		t.Fatalf("top-ups = %v", got)
	}
	for _, at := range []time.Time{beforeMonthEnd, afterMonthEnd} {
		if got := f.monthToDate(acct, at); got != 0 {
			t.Fatalf("month to date at %v = %d, want 0 for a failed top-up", at, got)
		}
	}
}

func TestPendingAutoTopUpFailsWhenTheClaimMonthCapIsUsed(t *testing.T) {
	f, acct := capFixture(t)
	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("no top-up queued")
	}
	// An automatic top-up of the new month already used the whole cap of 25
	// USD (completed at 00:00:30 of January). The December top-up fits its
	// own month when queued but not January, where the claim checks it, so it
	// fails as obsolete without a Paddle call and without counting as a
	// failed automatic top-up.
	claimedBefore := afterMonthEnd.Add(-30 * time.Second)
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind, amount_usd_micros, status,
			create_time, update_time, cap_time)
		VALUES ('01900000-0000-7000-8000-0000000000c1', $1, 'auto', $2, 'completed', $3, $3, $3)`,
		acct, 25*usd, claimedBefore); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx(), `UPDATE credit_settings SET auto_top_up_consecutive_failures = 1 WHERE billing_account_id = $1`, acct); err != nil {
		t.Fatal(err)
	}
	f.claimAfterMonthRollover(acct, lockCapAccount)
	if charges := f.paddle.Charges(); len(charges) != 0 {
		t.Fatalf("an obsolete top-up called Paddle: %+v", charges)
	}
	if got := f.topUpStatuses(acct); len(got) != 2 || got[0] != "failed:settings_changed" || got[1] != "completed" {
		t.Fatalf("top-ups = %v", got)
	}
	st, err := loadSettings(ctx(), f.db.Pool, acct)
	if err != nil {
		t.Fatal(err)
	}
	if !st.AutoEnabled || st.ConsecutiveFailures != 1 {
		t.Fatalf("the obsolete top-up changed the failure count or disabled automatic top-ups: %+v", st)
	}
	var notifications int
	if err := f.db.Pool.QueryRow(ctx(), `SELECT count(*) FROM billing_notifications
		WHERE billing_account_id = $1 AND kind IN ($2, $3)`, acct, NotifyPaymentFailed, NotifyAutoTopUpDisabled).
		Scan(&notifications); err != nil {
		t.Fatal(err)
	}
	if notifications != 0 {
		t.Fatalf("the obsolete top-up queued %d failure notifications", notifications)
	}
	// Only the completed January top-up counts in January.
	if got := f.monthToDate(acct, afterMonthEnd); got != 25*usd {
		t.Fatalf("January month to date = %d", got)
	}
}

func TestMonthToDateFallsBackToCreateTimeWithoutCapTime(t *testing.T) {
	f, acct := capFixture(t)
	// A row that exists before the cap time column has no cap time, and a
	// manual top-up never has one; the first counts by create_time, the
	// second not at all.
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind, amount_usd_micros, status,
			create_time, update_time)
		VALUES ('01900000-0000-7000-8000-0000000000a1', $1, 'auto', $2, 'completed', $3, $3)`,
		acct, 25*usd, beforeMonthEnd); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Pool.Exec(ctx(), `INSERT INTO top_ups (top_up_id, billing_account_id, kind, idempotency_key, amount_usd_micros, status,
			create_time, update_time)
		VALUES ('01900000-0000-7000-8000-0000000000a2', $1, 'manual', '01900000-0000-7000-8000-0000000000b2', $2, 'completed', $3, $3)`,
		acct, 100*usd, afterMonthEnd); err != nil {
		t.Fatal(err)
	}
	if got := f.monthToDate(acct, beforeMonthEnd); got != 25*usd {
		t.Fatalf("December month to date = %d", got)
	}
	if got := f.monthToDate(acct, afterMonthEnd); got != 0 {
		t.Fatalf("January month to date = %d, want 0 (manual top-ups are outside the cap)", got)
	}
}
