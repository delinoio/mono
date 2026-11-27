// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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

func TestAutoTopUpClaimDeadlineKeepsPending(t *testing.T) {
	for _, scenario := range []struct {
		name     string
		lock     string
		cancel   bool
		periodic bool
	}{
		{name: "account lock and later tasks", lock: lockCapAccount, periodic: true},
		{name: "top-up lock", lock: lockCapTopUp},
		{name: "parent cancellation", lock: lockCapAccount, cancel: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, acct := capFixture(t)
			if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
				t.Fatal("no top-up queued")
			}
			notice := f.queueNotice(f.account(), "credit_expiry/claim-lock")
			snapshot := func() string {
				t.Helper()
				var row string
				if err := f.db.Pool.QueryRow(t.Context(), `SELECT row_to_json(t)::text FROM top_ups t
					WHERE billing_account_id = $1`, acct).Scan(&row); err != nil {
					t.Fatal(err)
				}
				return row
			}
			before := snapshot()
			f.clock.Set(afterMonthEnd)
			f.s.autoTopUpBudget = time.Second
			if scenario.cancel {
				f.s.autoTopUpBudget = autoTopUpBudget
			}
			var logs bytes.Buffer
			f.s.logger = slog.New(slog.NewJSONHandler(&logs, nil))

			waitCtx, stopWaiting := context.WithTimeout(t.Context(), 10*time.Second)
			defer stopWaiting()
			holder, err := f.db.Pool.Begin(waitCtx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(context.Background()) }()
			var holderPID int
			if err := holder.QueryRow(waitCtx, scenario.lock, acct).Scan(&holderPID); err != nil {
				t.Fatal(err)
			}

			runCtx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				if scenario.periodic {
					f.s.RunOnce(runCtx)
					done <- nil
					return
				}
				done <- f.s.ProcessAutoTopUps(runCtx)
			}()
			waitForClaimLock(t, waitCtx, f, holderPID, done)
			if scenario.cancel {
				cancel()
			}
			select {
			case err := <-done:
				if scenario.cancel && !errors.Is(err, context.Canceled) || !scenario.cancel && err != nil {
					t.Fatalf("worker returned %v while holder was active", err)
				}
			case <-waitCtx.Done():
				t.Fatal("worker did not return while lock holder was active")
			}
			loggedBudget := strings.Contains(logs.String(), "billing_auto_top_up_budget_spent")
			if loggedBudget == scenario.cancel ||
				(loggedBudget && (!strings.Contains(logs.String(), `"budget_ms":1000`) || !strings.Contains(logs.String(), `"elapsed_ms":`))) {
				t.Fatalf("unexpected worker logs: %s", &logs)
			}
			if len(f.paddle.Charges()) != 0 || snapshot() != before {
				t.Fatal("blocked claim modified the top-up or called Paddle")
			}
			if scenario.periodic && !f.noticeState(notice).Sent {
				t.Fatal("independent notice did not send while the claim lock remained held")
			}
			if err := holder.Commit(waitCtx); err != nil {
				t.Fatal(err)
			}
			if err := f.s.ProcessAutoTopUps(waitCtx); err != nil {
				t.Fatal(err)
			}
			wantStatuses(t, "retried account", f.topUpStatuses(acct), "charged")
			if len(f.paddle.Charges()) != 1 || f.monthToDate(acct, afterMonthEnd) != 25*usd {
				t.Fatal("retry did not charge exactly once in the new cap month")
			}
		})
	}
}

func TestAutoTopUpClaimPropagatesDatabaseError(t *testing.T) {
	f, acct := capFixture(t)
	f.deduct(acct, 25*usd)
	if _, err := f.db.Pool.Exec(t.Context(), `ALTER TABLE credit_settings RENAME TO unavailable_credit_settings`); err != nil {
		t.Fatal(err)
	}
	var dbErr *pgconn.PgError
	err := f.s.ProcessAutoTopUps(t.Context())
	if !errors.As(err, &dbErr) || dbErr.Code != "42P01" {
		t.Fatalf("claim error = %v, want undefined table", err)
	}
	wantStatuses(t, "failed claim", f.topUpStatuses(acct), "pending")
	if len(f.paddle.Charges()) != 0 {
		t.Fatal("failed claim called Paddle")
	}
}

func TestAutoTopUpClaimPropagatesCommitDeadline(t *testing.T) {
	f, acct := capFixture(t)
	f.deduct(acct, 25*usd)
	f.s.autoTopUpBudget = time.Second
	// The deferred trigger starts only once PostgreSQL receives COMMIT.
	if _, err := f.db.Pool.Exec(t.Context(), `
		CREATE FUNCTION delay_claim_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			PERFORM pg_sleep(3);
			RETURN NEW;
		END;
		$$;
		CREATE CONSTRAINT TRIGGER delay_claim_commit AFTER UPDATE ON top_ups
		DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
		WHEN (NEW.status = 'charging') EXECUTE FUNCTION delay_claim_commit();
	`); err != nil {
		t.Fatal(err)
	}
	err := f.s.ProcessAutoTopUps(t.Context())
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "commit automatic top-up claim") {
		t.Fatalf("claim error = %v, want commit deadline", err)
	}
	if len(f.paddle.Charges()) != 0 {
		t.Fatal("an uncertain claim called Paddle")
	}
}
