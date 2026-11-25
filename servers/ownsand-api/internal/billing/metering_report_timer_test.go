// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

// guardTimeout only bounds how long a broken test waits; a passing test never
// waits for it.
const guardTimeout = 30 * time.Second

// fakeReportTimer is a reporter idle timer that fires only when the test says
// so and reports every Reset to the test. It uses no clock.
type fakeReportTimer struct {
	fire   chan time.Time
	resets chan time.Duration
}

func (t *fakeReportTimer) C() <-chan time.Time { return t.fire }
func (t *fakeReportTimer) Stop() bool          { return true }
func (t *fakeReportTimer) Reset(d time.Duration) bool {
	t.resets <- d
	return true
}

// Fire fires the timer. The channel holds one tick, so the reporter does not need
// to wait for it at that moment.
func (t *fakeReportTimer) Fire(tb testing.TB) {
	tb.Helper()
	select {
	case t.fire <- time.Time{}:
	default:
		tb.Fatal("the previous timer tick was not consumed")
	}
}

// NextReset waits until the reporter finished a pass and armed the timer.
func (t *fakeReportTimer) NextReset(tb testing.TB) time.Duration {
	tb.Helper()
	select {
	case d := <-t.resets:
		return d
	case <-time.After(guardTimeout):
		tb.Fatal("timed out waiting for the reporter to re-arm its timer")
		return 0
	}
}

func runWithFakeReportTimer(t *testing.T, m *billing.Metering) (*fakeReportTimer, func()) {
	t.Helper()
	timer := &fakeReportTimer{fire: make(chan time.Time, 1), resets: make(chan time.Duration, 8)}
	created := make(chan struct{})
	billing.SetReportTimerForTest(m, func(time.Duration) billing.TestReportTimer {
		close(created)
		return timer
	})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { m.Run(ctx); close(done) }()
	stop := func() { cancel(); <-done }
	t.Cleanup(stop)
	select {
	case <-created:
	case <-time.After(guardTimeout):
		t.Fatal("timed out waiting for the reporter to create its timer")
	}
	return timer, stop
}

// A new lease holder reports the due rows that another instance sealed, even
// when it seals no window itself, on the first idle timer pass after the
// lease handover. The test replaces the idle timer of the reporter with a
// fake that it fires explicitly, so the order of the steps does not depend on
// elapsed time. The Tick and Report intervals are long and differ, so the
// real tickers of the other workers never fire and the Reset duration shows
// which interval the reporter chose.
func TestRunReportsDueRowsAfterLeaseHandover(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	accounts, client := newFakeAccounts(t)
	ctx := context.Background()

	// A previous holder sealed the window and never reported it.
	previous := f.metering(nil)
	previous.Add(org1, classA, t0.Add(time.Minute), 2)
	if err := previous.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	f.clock.Set(t0.Add(7 * time.Minute))
	if n, err := previous.Seal(ctx); err != nil || n != 1 {
		t.Fatalf("seal: %d %v", n, err)
	}

	timing := billing.Timing{Flush: time.Hour, Tick: time.Hour, Report: 2 * time.Hour, BackoffMin: time.Second, BackoffMax: time.Minute}
	var held atomic.Bool
	m := f.metering(client, func(o *billing.Options) {
		o.Timing = &timing
		o.Held = held.Load
	})
	timer, _ := runWithFakeReportTimer(t, m)

	// Without the lease a pass reports nothing and checks the lease again
	// after one Tick. The reporter arms the timer after the lease check, so
	// the report count is final when the Reset arrives.
	timer.Fire(t)
	if d := timer.NextReset(t); d != timing.Tick {
		t.Fatalf("idle reset without the lease = %s, want the Tick %s", d, timing.Tick)
	}
	if _, reports := accounts.count(); reports != 0 {
		t.Fatalf("an instance without the lease reported %d rows", reports)
	}
	if s, ok := f.outbox(t, org1, classA, t0); !ok || s.reported {
		t.Fatalf("class A row after a pass without the lease: %+v %v", s, ok)
	}

	// Acquiring the lease drains the sealed row on the next pass and the
	// reporter then waits the configured idle Report interval.
	held.Store(true)
	timer.Fire(t)
	if d := timer.NextReset(t); d != timing.Report {
		t.Fatalf("idle reset of the lease holder = %s, want the Report %s", d, timing.Report)
	}
	if accounts.report(org1, accountsv1.Meter_METER_OWNSAND_CLASS_A_RPC, t0) == nil {
		t.Fatal("the sealed row was not reported after the handover")
	}
	if s, _ := f.outbox(t, org1, classA, t0); !s.reported {
		t.Fatal("the reported row is not marked reported")
	}

	// A due row that appears without a kick waits for the idle timer. The
	// timer is held, so no pass can run until the test fires it.
	_, before := accounts.count()
	if _, err := f.db.Pool.Exec(ctx, `INSERT INTO usage_outbox (organization_id, meter, window_start, quantity, amount_usd_micros, seal_time, next_attempt_time)
		SELECT organization_id, $1, window_start, quantity, amount_usd_micros, seal_time, seal_time FROM usage_outbox WHERE organization_id = $2 AND meter = $3 AND window_start = $4`,
		int16(classB), org1, int16(classA), t0); err != nil {
		t.Fatal(err)
	}
	if _, after := accounts.count(); after != before {
		t.Fatalf("the holder reported %d rows before its idle timer fired", after-before)
	}
	if s, ok := f.outbox(t, org1, classB, t0); !ok || s.reported {
		t.Fatalf("class B row before the idle timer fired: %+v %v", s, ok)
	}

	// When the idle timer fires, the holder reports the new row.
	timer.Fire(t)
	timer.NextReset(t)
	if accounts.report(org1, accountsv1.Meter_METER_OWNSAND_CLASS_B_RPC, t0) == nil {
		t.Fatal("the due row was not reported when the idle timer fired")
	}
	if s, _ := f.outbox(t, org1, classB, t0); !s.reported {
		t.Fatal("the class B row is not marked reported")
	}
}

func TestRunReportsDueRowsAfterLeaseReacquisition(t *testing.T) {
	f := newFixture(t, t0.Add(time.Minute))
	accounts, client := newFakeAccounts(t)
	ctx := t.Context()
	timing := billing.Timing{Flush: time.Hour, Tick: time.Hour, Report: 2 * time.Hour, BackoffMin: time.Second, BackoffMax: time.Minute}
	opts := app.Options{DB: f.db, Logger: f.logger, Now: f.clock.Now, BillingTiming: &timing}
	b, err := app.New(opts)
	if err != nil {
		t.Fatal(err)
	}
	opts.UsageClient = client
	a, err := app.New(opts)
	if err != nil {
		t.Fatal(err)
	}

	takeLease := func(instance *app.App) func() {
		t.Helper()
		runCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		go func() { instance.Lease.Run(runCtx); close(done) }()
		stop := func() { cancel(); <-done }
		t.Cleanup(stop)
		waitCtx, cancelWait := context.WithTimeout(ctx, guardTimeout)
		defer cancelWait()
		if err := instance.Lease.Wait(waitCtx); err != nil {
			t.Fatalf("wait for lease: %v", err)
		}
		return stop
	}
	seal := func(window time.Time) {
		t.Helper()
		release := takeLease(b)
		defer release()
		b.Metering.Add(org1, classA, window.Add(time.Minute), 2)
		if err := b.Metering.Flush(ctx); err != nil {
			t.Fatal(err)
		}
		f.clock.Set(window.Add(7 * time.Minute))
		if n, err := b.Metering.Seal(ctx); err != nil || n != 1 {
			t.Fatalf("seal: %d %v", n, err)
		}
	}

	// Use the timer for the first pass so A reaches its long idle wait
	// regardless of whether the acquisition callback wakes the reporter.
	onAcquire := a.Lease.OnAcquire
	a.Lease.OnAcquire = a.Control.LeaseAcquired
	timer, stopReporter := runWithFakeReportTimer(t, a.Metering)
	release := takeLease(a)
	timer.Fire(t)
	if d := timer.NextReset(t); d != timing.Report {
		t.Fatalf("initial holder reset = %s, want %s", d, timing.Report)
	}
	release()
	a.Lease.OnAcquire = onAcquire
	seal(t0)

	// Keep the old idle timer unfired. Only the production lease callback can
	// wake the reporter because A has sealed nothing.
	release = takeLease(a)
	if d := timer.NextReset(t); d != timing.Report {
		t.Fatalf("reset after reacquisition = %s, want %s", d, timing.Report)
	}
	if requests, reports := accounts.count(); requests != 1 || reports != 1 {
		t.Fatalf("requests = %d, reports = %d after reacquisition, want 1 and 1", requests, reports)
	}
	if s, ok := f.outbox(t, org1, classA, t0); !ok || !s.reported {
		t.Fatalf("sealed row after reacquisition: %+v %v", s, ok)
	}
	release()
	stopReporter()

	// Queue an acquisition wake while A's reporter is stopped. Release the
	// lease before starting the reporter so it consumes the wake as a nonholder.
	window := t0.Add(5 * time.Minute)
	seal(window)
	before, ok := f.outbox(t, org1, classA, window)
	if !ok || before.reported || before.attempts != 0 {
		t.Fatalf("second sealed row: %+v %v", before, ok)
	}
	release = takeLease(a)
	release()
	timer, _ = runWithFakeReportTimer(t, a.Metering)
	if d := timer.NextReset(t); d != timing.Tick {
		t.Fatalf("reset after a stale wake = %s, want %s", d, timing.Tick)
	}
	if requests, reports := accounts.count(); requests != 1 || reports != 1 {
		t.Fatalf("requests = %d, reports = %d after a stale wake, want 1 and 1", requests, reports)
	}
	if after, ok := f.outbox(t, org1, classA, window); !ok || after != before {
		t.Fatalf("unsent row changed: before %+v, after %+v", before, after)
	}
}
