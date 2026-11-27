// SPDX-License-Identifier: FSL-1.1-ALv2

package billing_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.opentelemetry.io/otel/attribute"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/accounts/accountstest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

func TestRunSlowPollDoesNotDelayMaintenance(t *testing.T) {
	f := newStatusFixture(t, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	f.addOrg(t, org2)
	f.clock.Advance(billing.SuspendAfter - time.Second)
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.addPendingUnlink(t, org1)
	if _, err := f.db.Pool.Exec(context.Background(),
		"UPDATE billing_pending_unlinks SET next_attempt_time = $1 WHERE organization_id = $2",
		f.clock.Now().Add(time.Second), org1); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	defer func() { cancel(); workers.Wait() }()
	gate := f.fake.Hold(accountstest.MethodListBillingStatusChanges)
	defer gate.Release()
	followerMeter := newTestMeter(t)
	timing := billing.DefaultStatusTiming()
	timing.Tick = 20 * time.Millisecond
	timing.Poll = time.Hour
	for _, instance := range []struct {
		held  bool
		meter *testMeter
	}{{held: true, meter: f.registry}, {held: false, meter: followerMeter}} {
		worker := billing.NewStatuses(billing.StatusOptions{
			DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: f.fake.Client(), Metrics: instance.meter, Timing: &timing,
			Ownership: func() (uint64, bool) { return 0, instance.held },
		})
		workers.Go(func() { worker.Run(ctx) })
	}

	eventually(t, "the open Accounts poll", func() bool { return len(gate.Entered) == 1 })
	gauges := func(suspended, pending float64) bool {
		for _, meter := range []*testMeter{f.registry, followerMeter} {
			phase, phaseOK := meter.gauge(t, "ownsand_billing_organizations", attribute.String("phase", "suspended"))
			unlinks, unlinksOK := meter.gauge(t, "ownsand_billing_pending_unlinks")
			if !phaseOK || !unlinksOK || phase != suspended || unlinks != pending {
				return false
			}
		}
		return true
	}
	eventually(t, "both instances' initial gauges", func() bool { return gauges(0, 1) })
	f.clock.Advance(2 * time.Second)
	eventually(t, "the stored suspension and confirmed unlink while the poll is held", func() bool {
		_, _, pending := f.pendingUnlink(t, org1)
		return !pending && f.load(t, org2).Phase == billing.PhaseSuspended
	})
	eventually(t, "both instances' refreshed gauges", func() bool { return gauges(1, 0) })
	eventually(t, "the suspension and unlink logs", func() bool {
		logs := f.logText()
		return strings.Contains(logs, `"msg":"billing_status_changed"`) && strings.Contains(logs, `"source":"time"`) &&
			strings.Contains(logs, `"msg":"billing_unlink_confirmed"`)
	})
	if polls := f.fake.Calls(accountstest.MethodListBillingStatusChanges); polls != 1 {
		t.Fatalf("status polls while the first poll was held = %d, want 1", polls)
	}
	if unlinks := f.fake.Calls(accountstest.MethodUnlinkOrganization); unlinks != 1 {
		t.Fatalf("unlink calls while the poll was held = %d, want 1", unlinks)
	}
}

func TestRunSlowPollIsSingleFlight(t *testing.T) {
	const tick = 20 * time.Millisecond
	for _, tc := range []struct {
		name string
		poll time.Duration
		kick bool
	}{
		{name: "overdue interval", poll: 100 * time.Millisecond},
		{name: "kicks across lease loss", poll: time.Hour, kick: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newStatusFixture(t, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
			gate := f.fake.Hold(accountstest.MethodListBillingStatusChanges)
			var held atomic.Bool
			held.Store(true)
			timing := billing.DefaultStatusTiming()
			timing.Tick, timing.Poll = tick, tc.poll
			statuses, stop, _ := f.runStatuses(timing, f.fake.Client(), func() (uint64, bool) { return 0, held.Load() })
			defer stop()
			defer gate.Release()
			eventually(t, "the first poll at the Accounts endpoint", func() bool { return len(gate.Entered) == 1 })

			if tc.kick {
				for range 5 {
					statuses.KickPoll()
					time.Sleep(tick)
				}
			} else {
				time.Sleep(3 * tc.poll)
			}
			polls := func() int { return f.fake.Calls(accountstest.MethodListBillingStatusChanges) }
			if got := polls(); got != 1 {
				t.Fatalf("poll requests while the first was still active = %d, want 1", got)
			}
			if tc.kick {
				held.Store(false)
			}
			gate.Release()

			if tc.kick {
				eventually(t, "the finished first poll", func() bool {
					return strings.Contains(f.logText(), `"msg":"billing_status_polled"`)
				})
				time.Sleep(5 * tick)
				if got := polls(); got != 1 {
					t.Fatalf("poll requests without the lease = %d, want 1", got)
				}
				held.Store(true)
				eventually(t, "the coalesced follow-up after reacquiring the lease", func() bool { return polls() == 2 })
				time.Sleep(5 * tick)
				if got := polls(); got != 2 {
					t.Fatalf("poll requests after multiple kicks = %d, want 2", got)
				}
			} else {
				eventually(t, "the next interval poll after release", func() bool { return polls() >= 2 })
			}
		})
	}
}

// cancelingPollClient waits for the test to let a canceled poll exit.
type cancelingPollClient struct {
	accountsv1connect.UsageServiceClient
	canceled chan error
	exit     <-chan struct{}
}

func (c cancelingPollClient) ListBillingStatusChanges(ctx context.Context, req *connect.Request[accountsv1.ListBillingStatusChangesRequest]) (*connect.Response[accountsv1.ListBillingStatusChangesResponse], error) {
	resp, err := c.UsageServiceClient.ListBillingStatusChanges(ctx, req)
	if ctx.Err() != nil {
		c.canceled <- ctx.Err()
		<-c.exit
	}
	return resp, err
}

func TestRunCancelsAndJoinsSlowPoll(t *testing.T) {
	f := newStatusFixture(t, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	gate := f.fake.Hold(accountstest.MethodListBillingStatusChanges)
	defer gate.Release()
	exit := make(chan struct{})
	letPollExit := sync.OnceFunc(func() { close(exit) })
	client := cancelingPollClient{UsageServiceClient: f.fake.Client(), canceled: make(chan error, 1), exit: exit}
	timing := billing.DefaultStatusTiming()
	timing.Tick = 20 * time.Millisecond
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Client: client, Timing: &timing})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { statuses.Run(ctx); close(done) }()
	defer func() { cancel(); letPollExit(); <-done }()

	eventually(t, "the held poll before shutdown", func() bool { return len(gate.Entered) == 1 })
	statuses.KickPoll()
	cancel()
	select {
	case err := <-client.canceled:
		if err != context.Canceled {
			t.Fatalf("poll context error = %v, want context.Canceled", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("canceling Run did not interrupt the poll")
	}
	select {
	case <-done:
		t.Fatal("Run returned before the canceled poll exited")
	case <-time.After(5 * timing.Tick):
	}
	letPollExit()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after the canceled poll exited")
	}
	if got := f.fake.Calls(accountstest.MethodListBillingStatusChanges); got != 1 {
		t.Fatalf("poll requests after cancellation = %d, want 1", got)
	}
}

func TestRunMaintainsLocalStatusWithoutAccounts(t *testing.T) {
	f := newStatusFixture(t, time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC))
	f.addOrg(t, org1)
	f.link(t, org1, accountA)
	f.addPendingUnlink(t, org1)
	f.clock.Advance(billing.SuspendAfter)
	timing := billing.DefaultStatusTiming()
	timing.Tick = 20 * time.Millisecond
	statuses := billing.NewStatuses(billing.StatusOptions{DB: f.db, Logger: f.logger, Now: f.clock.Now, Metrics: f.registry, Timing: &timing})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { statuses.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	statuses.KickPoll()

	eventually(t, "the stored suspension and gauges without an Accounts client", func() bool {
		suspended, phaseOK := f.registry.gauge(t, "ownsand_billing_organizations", attribute.String("phase", "suspended"))
		unlinks, unlinksOK := f.registry.gauge(t, "ownsand_billing_pending_unlinks")
		return phaseOK && unlinksOK && suspended == 1 && unlinks == 1 && f.load(t, org1).Phase == billing.PhaseSuspended
	})
	if _, _, ok := f.pendingUnlink(t, org1); !ok {
		t.Fatal("the due unlink was removed without an Accounts client")
	}
}
