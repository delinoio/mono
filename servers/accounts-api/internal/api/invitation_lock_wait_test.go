// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

// Fixture for the invitation tests that need the acceptance to block on the
// billing account lock. The fixture moves the clock and releases the lock
// holder only after it has observed the acceptance statement waiting on the
// holder's backend, and it releases the holder by rollback when it cannot
// observe that, so a missing or unrelated waiter can never pass a test.

// lockWaitDeadline bounds the observation. It only limits how long a broken
// fixture can block the RPC; a working fixture returns as soon as it sees the
// waiter.
const lockWaitDeadline = 30 * time.Second

// errBlockedAcceptanceNotObserved reports that the observation ended, because
// its context ended, before it saw the acceptance statement blocked by the
// holder.
var errBlockedAcceptanceNotObserved = errors.New("the acceptance statement was not observed blocked by the lock holder")

// blockedAcceptanceObserver reports whether a statement of the acceptance is
// waiting for a lock that the backend with the given process ID holds.
type blockedAcceptanceObserver func(ctx context.Context, holderPID int32) (bool, error)

// acceptanceLockStatement matches the statement of AcceptInvitation that locks
// the billing account (a LIKE pattern over pg_stat_activity.query).
const acceptanceLockStatement = `%SELECT name, create_time FROM billing_accounts%FOR UPDATE%`

// pgBlockedAcceptanceObserver observes pg_stat_activity of the current
// database. A match needs a backend that waits for a lock, that runs the
// acceptance lock statement and that the holder backend blocks.
func pgBlockedAcceptanceObserver(pool *pgxpool.Pool) blockedAcceptanceObserver {
	return func(ctx context.Context, holderPID int32) (bool, error) {
		var blocked bool
		err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity a
			WHERE a.datname = current_database()
			AND a.pid <> pg_backend_pid()
			AND a.wait_event_type = 'Lock'
			AND a.query LIKE $1
			AND $2::int = ANY (pg_blocking_pids(a.pid)))`, acceptanceLockStatement, holderPID).Scan(&blocked)
		return blocked, err
	}
}

// waitForBlockedAcceptance polls observe until it reports true. It returns
// the error of observe, or errBlockedAcceptanceNotObserved joined with the
// reason of the ended context. pause is the delay between two polls.
func waitForBlockedAcceptance(ctx context.Context, observe blockedAcceptanceObserver, holderPID int32, pause func(context.Context)) error {
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(errBlockedAcceptanceNotObserved, err)
		}
		blocked, err := observe(ctx, holderPID)
		if err != nil {
			return err
		}
		if blocked {
			return nil
		}
		pause(ctx)
	}
}

// pauseBetweenPolls waits for a short interval or until ctx ends.
func pauseBetweenPolls(ctx context.Context) {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// lockHolderTx is the part of pgx.Tx that the fixture ends the holder with.
type lockHolderTx interface {
	Commit(ctx context.Context) error
	Rollback(ctx context.Context) error
}

// lockHolderResult separates the outcome of the fixture from the RPC result.
// A test checks both. waitErr is set when the fixture could not observe the
// blocked acceptance; then the holder was rolled back and the clock is
// unchanged.
type lockHolderResult struct {
	waitErr   error
	commitErr error
}

// releaseOnVerifiedWait waits until the acceptance is observed blocked by the
// holder, then calls advance (the clock move) and commits the holder. When the
// wait fails, it rolls the holder back without calling advance, so the
// blocked RPC finishes.
func releaseOnVerifiedWait(ctx context.Context, tx lockHolderTx, holderPID int32,
	observe blockedAcceptanceObserver, pause func(context.Context), advance func()) lockHolderResult {
	if err := waitForBlockedAcceptance(ctx, observe, holderPID, pause); err != nil {
		// The context of the wait may have ended; the rollback must not
		// depend on it.
		return lockHolderResult{waitErr: err, commitErr: tx.Rollback(context.Background())}
	}
	advance()
	return lockHolderResult{commitErr: tx.Commit(context.Background())}
}

// holdAccountLock starts a transaction that locks the billing account and
// returns a channel with exactly one result. The holder is committed after
// advance, once the acceptance is observed waiting for the lock. It is meant
// to be called from a directory hook, so the lock is held before the
// acceptance transaction starts.
func holdAccountLock(env *testenv.Env, accountID string, advance func()) <-chan lockHolderResult {
	done := make(chan lockHolderResult, 1)
	tx, err := env.DB.Pool.Begin(context.Background())
	if err != nil {
		done <- lockHolderResult{waitErr: err}
		return done
	}
	fail := func(err error) <-chan lockHolderResult {
		done <- lockHolderResult{waitErr: err, commitErr: tx.Rollback(context.Background())}
		return done
	}
	if _, err := tx.Exec(context.Background(), `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`, accountID); err != nil {
		return fail(err)
	}
	var pid int32
	if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		return fail(err)
	}
	go func() {
		waitCtx, cancel := context.WithTimeout(context.Background(), lockWaitDeadline)
		defer cancel()
		done <- releaseOnVerifiedWait(waitCtx, tx, pid, pgBlockedAcceptanceObserver(env.DB.Pool), pauseBetweenPolls, advance)
	}()
	return done
}

// fakeLockHolderTx records how the fixture ends the holder.
type fakeLockHolderTx struct {
	events    *[]string
	commitErr error
}

func (f fakeLockHolderTx) Commit(context.Context) error {
	*f.events = append(*f.events, "commit")
	return f.commitErr
}

func (f fakeLockHolderTx) Rollback(context.Context) error {
	*f.events = append(*f.events, "rollback")
	return nil
}

func noPause(context.Context) {}

func TestReleaseOnVerifiedWaitAdvancesBeforeCommit(t *testing.T) {
	var events []string
	polls := 0
	observe := func(_ context.Context, pid int32) (bool, error) {
		polls++
		if pid != 4242 {
			t.Errorf("observed holder pid %d, want 4242", pid)
		}
		return polls == 3, nil
	}
	got := releaseOnVerifiedWait(context.Background(), fakeLockHolderTx{events: &events}, 4242, observe, noPause,
		func() { events = append(events, "advance") })
	if got.waitErr != nil || got.commitErr != nil {
		t.Fatalf("result = %+v, want no error", got)
	}
	if len(events) != 2 || events[0] != "advance" || events[1] != "commit" || polls != 3 {
		t.Fatalf("events = %v after %d polls, want [advance commit] after 3", events, polls)
	}
}

func TestReleaseOnVerifiedWaitReportsCommitError(t *testing.T) {
	var events []string
	commitErr := errors.New("commit failed")
	got := releaseOnVerifiedWait(context.Background(), fakeLockHolderTx{events: &events, commitErr: commitErr}, 1,
		func(context.Context, int32) (bool, error) { return true, nil }, noPause, func() {})
	if got.waitErr != nil || !errors.Is(got.commitErr, commitErr) {
		t.Fatalf("result = %+v, want only the commit error", got)
	}
}

// Without any matching waiter the observation ends with its context. The
// holder is rolled back, the clock does not move and the timeout is reported.
func TestReleaseOnVerifiedWaitWithoutWaiterRollsBack(t *testing.T) {
	var events []string
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	polls := 0
	observe := func(context.Context, int32) (bool, error) {
		polls++
		if polls == 3 {
			cancel() // Stands in for the deadline: the waiter never shows up.
		}
		return false, nil
	}
	got := releaseOnVerifiedWait(ctx, fakeLockHolderTx{events: &events}, 1, observe, noPause,
		func() { events = append(events, "advance") })
	if !errors.Is(got.waitErr, errBlockedAcceptanceNotObserved) || !errors.Is(got.waitErr, context.Canceled) {
		t.Fatalf("waitErr = %v, want the missing waiter with its context error", got.waitErr)
	}
	if got.commitErr != nil {
		t.Fatalf("rollback error = %v", got.commitErr)
	}
	if len(events) != 1 || events[0] != "rollback" {
		t.Fatalf("events = %v, want only [rollback]", events)
	}
}

// An observation that fails must not be read as a wait.
func TestReleaseOnVerifiedWaitObservationErrorRollsBack(t *testing.T) {
	var events []string
	queryErr := errors.New("pg_stat_activity query failed")
	got := releaseOnVerifiedWait(context.Background(), fakeLockHolderTx{events: &events}, 1,
		func(context.Context, int32) (bool, error) { return false, queryErr }, noPause,
		func() { events = append(events, "advance") })
	if !errors.Is(got.waitErr, queryErr) || errors.Is(got.waitErr, errBlockedAcceptanceNotObserved) {
		t.Fatalf("waitErr = %v, want the observation error itself", got.waitErr)
	}
	if len(events) != 1 || events[0] != "rollback" {
		t.Fatalf("events = %v, want only [rollback]", events)
	}
}

// A context that ended before the first poll never reports a wait.
func TestWaitForBlockedAcceptanceEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := waitForBlockedAcceptance(ctx, func(context.Context, int32) (bool, error) {
		called = true
		return true, nil
	}, 1, noPause)
	if !errors.Is(err, errBlockedAcceptanceNotObserved) || called {
		t.Fatalf("err = %v, observed = %v, want the missing waiter without a poll", err, called)
	}
}

// backendBlockedOn starts a backend that runs statement and blocks on a lock,
// and returns once Postgres reports that backend waiting. The returned channel
// receives the error of the statement after the lock holder ended. The
// backend is released and joined by the test cleanup.
func backendBlockedOn(t *testing.T, pool *pgxpool.Pool, statement string, args ...any) <-chan error {
	t.Helper()
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire the waiter connection: %v", err)
	}
	var pid int32
	if err := conn.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		conn.Release()
		t.Fatalf("read the waiter backend pid: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		defer conn.Release()
		_, err := conn.Exec(context.Background(), statement, args...)
		done <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), lockWaitDeadline)
	defer cancel()
	for {
		var waiting bool
		if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
			WHERE pid = $1 AND wait_event_type = 'Lock')`, pid).Scan(&waiting); err != nil {
			t.Fatalf("read the waiter state: %v", err)
		}
		if waiting {
			return done
		}
		pauseBetweenPolls(ctx)
	}
}

// The observer matches the acceptance lock statement blocked by the holder
// backend, and neither a different statement blocked by the holder nor the
// acceptance statement blocked by another backend.
func TestPgBlockedAcceptanceObserverMatchesOnlyTheAcceptanceWaiter(t *testing.T) {
	env := testenv.New(t)
	owner := newPerson(env, "owner")
	accountID := createAccount(t, owner, "Team").GetBillingAccountId()
	pool := env.DB.Pool
	observe := pgBlockedAcceptanceObserver(pool)

	holderTx := func() (pgx.Tx, int32) {
		tx, err := pool.Begin(context.Background())
		if err != nil {
			t.Fatalf("begin a lock holder: %v", err)
		}
		// A no-op after the test released the holder.
		t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
		if _, err := tx.Exec(context.Background(), `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`, accountID); err != nil {
			t.Fatalf("hold the account lock: %v", err)
		}
		var pid int32
		if err := tx.QueryRow(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			t.Fatalf("read the holder backend pid: %v", err)
		}
		return tx, pid
	}
	check := func(label string, pid int32, want bool) {
		t.Helper()
		got, err := observe(context.Background(), pid)
		if err != nil {
			t.Fatalf("%s: observe: %v", label, err)
		}
		if got != want {
			t.Fatalf("%s: observed = %v, want %v", label, got, want)
		}
	}

	tx, pid := holderTx()
	check("nothing waits", pid, false)

	// A waiter that locks the same row with a different statement. It ends
	// before the next phase, because a row lock waiter queued behind another
	// waiter is blocked by that waiter and not by the holder.
	unrelated := backendBlockedOn(t, pool, `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 FOR UPDATE`, accountID)
	check("unrelated statement", pid, false)
	release := func(tx pgx.Tx, waiter <-chan error) {
		t.Helper()
		if err := tx.Rollback(context.Background()); err != nil {
			t.Fatalf("release the holder: %v", err)
		}
		if err := <-waiter; err != nil {
			t.Fatalf("waiter statement: %v", err)
		}
	}
	release(tx, unrelated)

	// The acceptance statement, blocked by a new holder.
	tx, pid = holderTx()
	acceptance := backendBlockedOn(t, pool, `SELECT name, create_time FROM billing_accounts
		WHERE billing_account_id = $1 AND delete_time IS NULL FOR UPDATE`, accountID)
	check("acceptance statement blocked by the holder", pid, true)
	// The same waiter is not blocked by a backend that holds nothing.
	check("acceptance statement, other holder", pid+1000000, false)
	release(tx, acceptance)
	check("after the release", pid, false)
}
