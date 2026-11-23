// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// Values of the budget tests. They are virtual: budgetClock moves only when a
// charge is sent, so no test waits for real time.
const (
	budgetTestBudget     = 30 * time.Second
	budgetTestChargeTime = 20 * time.Second
)

// budgetClock is the fake monotonic clock of ProcessAutoTopUps.
type budgetClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *budgetClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *budgetClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// slowCharges is an HTTP transport that makes every one-time charge take
// duration of the fake clock, and runs onCharge (if set) while the charge is
// in flight.
type slowCharges struct {
	clock    *budgetClock
	duration time.Duration
	onCharge func()
	next     http.RoundTripper
}

func (c *slowCharges) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/charge") {
		c.clock.Advance(c.duration)
		if c.onCharge != nil {
			c.onCharge()
		}
	}
	return c.next.RoundTrip(req)
}

// slowQueueFixture returns a fixture with n accounts that each have a queued
// automatic top-up, and a Paddle client whose charges each use
// budgetTestChargeTime of the budget. The returned accounts are in queue
// order.
func slowQueueFixture(t *testing.T, n int, onCharge func()) (*fixture, []string) {
	t.Helper()
	f := newFixture(t)
	clock := &budgetClock{now: time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)}
	f.s.elapsed = clock.Now
	f.s.autoTopUpBudget = budgetTestBudget
	f.s.paddle = paddle.New(paddle.Config{
		BaseURL: f.paddle.URL,
		APIKey:  paddletest.APIKey,
		Client: &http.Client{Transport: &slowCharges{
			clock: clock, duration: budgetTestChargeTime, onCharge: onCharge, next: http.DefaultTransport,
		}},
	})
	accounts := make([]string, n)
	for i := range accounts {
		acct := f.account()
		f.activate(acct)
		f.grant(acct, LedgerManualTopUp, 30*usd)
		f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
		if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
			t.Fatalf("account %d: a deduction below the threshold queued no top-up", i)
		}
		accounts[i] = acct
		// Distinct queue times fix the order of the queue.
		f.clock.Advance(time.Second)
	}
	return f, accounts
}

// A queue of slow top-ups must not use the whole run: the run stops claiming
// top-ups once the budget is spent, the later tasks of the same RunOnce still
// run, the top-ups that were not claimed stay pending, and the next run
// charges them.
func TestRunOnceLeavesTimeForLaterTasksDuringASlowTopUpQueue(t *testing.T) {
	f, accounts := slowQueueFixture(t, 3, nil)
	// A later task of the run has work: a due credit expiry notice.
	notice := f.queueNotice(accounts[0], "credit_expiry/budget-test")

	f.s.RunOnce(ctx())

	// Two charges fit the budget: the second starts at 20s, before 30s, and
	// ends at 40s, after the budget. It still ends normally and records its
	// result.
	if n := len(f.paddle.Charges()); n != 2 {
		t.Fatalf("charges after the first run = %d, want 2", n)
	}
	wantStatuses(t, "first account", f.topUpStatuses(accounts[0]), "charged")
	wantStatuses(t, "second account", f.topUpStatuses(accounts[1]), "charged")
	wantStatuses(t, "third account", f.topUpStatuses(accounts[2]), "pending")
	if !f.noticeState(notice).Sent {
		t.Fatal("deliver_notifications did not run after the top-up queue used its budget")
	}

	f.s.RunOnce(ctx())

	if n := len(f.paddle.Charges()); n != 3 {
		t.Fatalf("charges after the second run = %d, want 3", n)
	}
	wantStatuses(t, "third account after the second run", f.topUpStatuses(accounts[2]), "charged")
}

// A charge that has started is not abandoned when the run context ends while
// the Paddle call is in flight: its outcome is recorded, and the top-ups that
// were not claimed stay pending.
func TestAutoTopUpInFlightChargeRecordsItsOutcomeWhenTheRunEnds(t *testing.T) {
	runCtx, cancel := context.WithCancel(ctx())
	defer cancel()
	f, accounts := slowQueueFixture(t, 2, cancel)

	err := f.s.ProcessAutoTopUps(runCtx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ProcessAutoTopUps error = %v, want context canceled", err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
	wantStatuses(t, "in-flight account", f.topUpStatuses(accounts[0]), "charged")
	wantStatuses(t, "unclaimed account", f.topUpStatuses(accounts[1]), "pending")
}
