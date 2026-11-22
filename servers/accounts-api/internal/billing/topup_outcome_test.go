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

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/accounts-api/internal/paddle"
	"github.com/delinoio/mono/servers/accounts-api/internal/paddle/paddletest"
)

// Deadlines of the outcome tests. The Paddle call gets callBudget and then
// times out; the account lock stays held for lockHold after the call started,
// which is after the call budget (the old shared deadline) ended and before
// the outcome budget (which starts when the call returns) ends.
const (
	outcomeTestCallBudget    = 300 * time.Millisecond
	outcomeTestLockHold      = 600 * time.Millisecond
	outcomeTestOutcomeBudget = 5 * time.Second
)

// lockOnCharge is an HTTP transport that, when a one-time charge is sent,
// locks the billing account from another connection and releases the lock
// after hold. So the lock is held from the start of the Paddle call, and the
// charge outcome write has to wait for it. The fake Paddle server behind it
// hangs until the client gives up, which the client sees as a timeout.
type lockOnCharge struct {
	f    *fixture
	acct string
	hold time.Duration
	// finalize runs in the locking transaction before it commits, like a
	// payment webhook that finalizes the top-up while the lock is held.
	finalize string

	next http.RoundTripper
	wg   sync.WaitGroup
}

func (l *lockOnCharge) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/charge") {
		tx, err := l.f.db.Pool.Begin(ctx())
		if err != nil {
			return nil, err
		}
		if _, err := lockAccount(ctx(), tx, l.acct); err != nil {
			_ = tx.Rollback(ctx())
			return nil, err
		}
		l.wg.Add(1)
		time.AfterFunc(l.hold, func() {
			defer l.wg.Done()
			if l.finalize != "" {
				if _, err := tx.Exec(ctx(), `UPDATE top_ups SET status = $2 WHERE billing_account_id = $1`, l.acct, l.finalize); err != nil {
					l.f.t.Errorf("finalize top-up: %v", err)
				}
			}
			if err := tx.Commit(ctx()); err != nil {
				l.f.t.Errorf("release the account lock: %v", err)
			}
		})
	}
	return l.next.RoundTrip(req)
}

// outcomeFixture returns a fixture with an active subscription whose Paddle
// charge hangs until the call budget ends while another connection holds the
// billing account lock.
func outcomeFixture(t *testing.T, hold time.Duration, outcomeBudget time.Duration, finalize string) (*fixture, string, *lockOnCharge) {
	t.Helper()
	f := newFixture(t)
	acct := f.account()
	f.activate(acct)
	f.paddle.FailCharges(&paddletest.Failure{Hang: true})
	lock := &lockOnCharge{f: f, acct: acct, hold: hold, finalize: finalize, next: http.DefaultTransport}
	f.s.paddle = paddle.New(paddle.Config{
		BaseURL: f.paddle.URL,
		APIKey:  paddletest.APIKey,
		// No client timeout: only the call deadline of the service ends the call.
		Client: &http.Client{Transport: lock},
	})
	f.s.paddleCallTimeout = outcomeTestCallBudget
	f.s.outcomeTimeout = outcomeBudget
	t.Cleanup(lock.wg.Wait)
	return f, acct, lock
}

// topUpKey is the idempotency key of the manual top-ups in these tests.
const topUpKey = "0197f2c0-0000-7000-8000-000000000001"

func (f *fixture) manualTopUp(acct, key string) error {
	f.t.Helper()
	return f.s.CreateTopUp(ctx(), acct, "admin-user", key, 25*usd, func(pgx.Tx) error { return nil }, nil, nil)
}

func wantStatuses(t *testing.T, label string, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: top-up statuses = %v, want %v", label, got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: top-up statuses = %v, want %v", label, got, want)
		}
	}
}

// A Paddle call that times out leaves an unknown result. The write of that
// result waits for the billing account lock, and the wait may run past the
// deadline that the call used (the old shared deadline). The write has its
// own deadline, so the unknown result is still recorded, the caller still
// gets the provider-unavailable answer, and a retry with the same key sends
// no second charge.
func TestManualTopUpRecordsUnknownAfterTheCallDeadline(t *testing.T) {
	f, acct, _ := outcomeFixture(t, outcomeTestLockHold, outcomeTestOutcomeBudget, "")

	err := f.manualTopUp(acct, topUpKey)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("CreateTopUp error = %v, want unavailable", err)
	}
	wantStatuses(t, "after the timeout", f.topUpStatuses(acct), "unknown")
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}

	// The retry finds the unknown top-up and charges nothing again.
	err = f.manualTopUp(acct, topUpKey)
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("retry error = %v, want unavailable", err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges after the retry = %d, want 1", n)
	}
}

func TestAutoTopUpRecordsUnknownAfterTheCallDeadline(t *testing.T) {
	f, acct, _ := outcomeFixture(t, outcomeTestLockHold, outcomeTestOutcomeBudget, "")
	f.grant(acct, LedgerManualTopUp, 30*usd)
	f.enableAutoTopUp(acct, 10*usd, 25*usd, 100*usd)
	if res := f.deduct(acct, 25*usd); !res.AutoTopUpQueued {
		t.Fatal("a deduction below the threshold queued no top-up")
	}

	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatalf("ProcessAutoTopUps: %v", err)
	}
	wantStatuses(t, "after the timeout", f.topUpStatuses(acct), "unknown")
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}
	// The worker does not charge an unknown top-up again.
	if err := f.s.ProcessAutoTopUps(ctx()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges after the next run = %d, want 1", n)
	}
}

// A webhook that finalized the top-up while the outcome write waited stays
// authoritative: the conditional update does not overwrite it.
func TestTopUpOutcomeKeepsAFinalizedTopUp(t *testing.T) {
	f, acct, _ := outcomeFixture(t, outcomeTestLockHold, outcomeTestOutcomeBudget, "completed")

	err := f.manualTopUp(acct, topUpKey)
	var connectErr *connect.Error
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("CreateTopUp error = %v, want unavailable", err)
	}
	wantStatuses(t, "after the finalization", f.topUpStatuses(acct), "completed")
}

// A lock wait that outlasts the outcome deadline is a real database timeout:
// the error is returned instead of the unavailable answer, and the top-up
// stays charging for the top-up timeout to resolve.
func TestTopUpOutcomeDeadlineStillReturnsTheDatabaseError(t *testing.T) {
	f, acct, lock := outcomeFixture(t, 2*time.Second, 300*time.Millisecond, "")

	err := f.manualTopUp(acct, topUpKey)
	var connectErr *connect.Error
	if err == nil || errors.As(err, &connectErr) {
		t.Fatalf("CreateTopUp error = %v, want a database error", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CreateTopUp error = %v, want a deadline error", err)
	}
	// Wait for the lock holder before reading the state.
	lock.wg.Wait()
	wantStatuses(t, "after the database timeout", f.topUpStatuses(acct), "charging")
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges = %d, want 1", n)
	}

	// A retry with the same key finds the open top-up and sends no charge.
	err = f.manualTopUp(acct, topUpKey)
	if !errors.As(err, &connectErr) || connectErr.Code() != connect.CodeUnavailable {
		t.Fatalf("retry error = %v, want unavailable", err)
	}
	if n := len(f.paddle.Charges()); n != 1 {
		t.Fatalf("charges after the retry = %d, want 1", n)
	}
}
