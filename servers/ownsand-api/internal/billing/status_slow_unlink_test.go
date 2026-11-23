// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

// TestRunSlowPendingUnlinkDoesNotDelayWorker checks that an UnlinkOrganization
// call that does not return does not hold back the status polls, the polls
// that a lease handover kicks or the recording of time-based suspensions:
// the retries of pending unlinks run apart from the other steps of the
// status worker.
func TestRunSlowPendingUnlinkDoesNotDelayWorker(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		poll time.Duration
		// kick is true when the poll interval is too long for anything but
		// KickPoll to cause the second poll.
		kick bool
	}{
		{"poll cadence", 100 * time.Millisecond, false},
		{"lease handover kick", time.Hour, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture(t, t0)
			f.addOrg(t, org1)
			f.link(t, org1, accountA)
			f.addPendingUnlink(t, org1)
			// org2 is unlinked and blocked from its creation: three days later,
			// Advance stores the suspension.
			f.addOrg(t, org2)
			gate := f.fake.Hold(accountstest.MethodUnlinkOrganization)
			timing := billing.StatusTiming{Poll: tc.poll, Tick: 20 * time.Millisecond, UnlinkBackoffMin: time.Hour, UnlinkBackoffMax: 2 * time.Hour}
			statuses, stop := f.runStatuses(timing, f.fake.Client(), nil)
			defer stop()
			// Registered after stop, so it runs first: the held call must end
			// before the worker is stopped.
			defer gate.Release()

			<-gate.Entered
			polls := func() int { return f.fake.Calls(accountstest.MethodListBillingStatusChanges) }
			eventually(t, "the poll of the first step", func() bool { return polls() >= 1 })
			base := polls()
			if tc.kick {
				statuses.KickPoll()
				eventually(t, "the poll after a kick while an unlink is in flight", func() bool { return polls() >= base+1 })
			} else {
				eventually(t, "two polls while an unlink is in flight", func() bool { return polls() >= base+2 })
			}

			f.clock.Advance(billing.SuspendAfter + time.Hour)
			eventually(t, "the time-based suspension while an unlink is in flight", func() bool {
				return f.load(t, org2).Phase == billing.PhaseSuspended
			})
			if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 1 {
				t.Fatalf("UnlinkOrganization calls = %d while the first call is held, want 1", n)
			}

			// The retries go on once the call returns.
			gate.Release()
			eventually(t, "the confirmed unlink", func() bool {
				_, _, ok := f.pendingUnlink(t, org1)
				return !ok
			})
			if _, _, linked := f.fake.Link(org1); linked {
				t.Fatal("delino-accounts still has the link")
			}
			if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 1 {
				t.Fatalf("UnlinkOrganization calls = %d, want 1: the pending unlink was sent twice", n)
			}
		})
	}
}

// TestRunStopsWithSlowPendingUnlink checks that Run waits for the retry batch
// that it started: it returns only after the batch ended, and the batch ends
// with the context of Run.
func TestRunStopsWithSlowPendingUnlink(t *testing.T) {
	f := newStatusFixture(t, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.addPendingUnlink(t, org1)
	gate := f.fake.Hold(accountstest.MethodUnlinkOrganization)
	defer gate.Release()
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: f.fake.Client(),
		Timing: &billing.StatusTiming{Poll: time.Hour, Tick: 20 * time.Millisecond, UnlinkBackoffMin: time.Hour, UnlinkBackoffMax: 2 * time.Hour}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { statuses.Run(ctx); close(done) }()
	<-gate.Entered
	cancel()
	// The call is held by the fake, but the client call ends with the
	// context; Run returns after the batch did.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after its context ended while an unlink was in flight")
	}
}

// TestRetryPendingUnlinksStopsWithoutLease checks that a retry batch sends
// nothing without the lease and stops at the next row when the lease is lost
// during the batch.
func TestRetryPendingUnlinksStopsWithoutLease(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC)
	f := newStatusFixture(t, t0)
	ctx := context.Background()
	f.addOrg(t, org1)
	f.addOrg(t, org2)
	f.link(t, org1, accountA)
	f.link(t, org2, accountA)
	// org1 is due earlier than org2, so the batch handles it first.
	f.addPendingUnlink(t, org1)
	f.clock.Advance(time.Second)
	f.addPendingUnlink(t, org2)
	f.clock.Advance(time.Second)

	var mu sync.Mutex
	isHeld := false
	setHeld := func(v bool) { mu.Lock(); isHeld = v; mu.Unlock() }
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: f.fake.Client(),
		Held: func() bool { mu.Lock(); defer mu.Unlock(); return isHeld }})

	if err := statuses.RetryPendingUnlinks(ctx); err != nil {
		t.Fatal(err)
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 0 {
		t.Fatalf("a batch without the lease sent %d unlinks", n)
	}

	setHeld(true)
	gate := f.fake.Hold(accountstest.MethodUnlinkOrganization)
	defer gate.Release()
	wait := goErr(func() error { return statuses.RetryPendingUnlinks(ctx) })
	<-gate.Entered
	setHeld(false)
	gate.Release()
	if err := wait(); err != nil {
		t.Fatal(err)
	}
	if n := f.fake.Calls(accountstest.MethodUnlinkOrganization); n != 1 {
		t.Fatalf("UnlinkOrganization calls = %d, want 1: the batch went on after the lease was lost", n)
	}
	if _, _, ok := f.pendingUnlink(t, org1); ok {
		t.Fatal("the first pending unlink stayed after a success")
	}
	if _, _, ok := f.pendingUnlink(t, org2); !ok {
		t.Fatal("the second pending unlink was handled after the lease was lost")
	}
}
