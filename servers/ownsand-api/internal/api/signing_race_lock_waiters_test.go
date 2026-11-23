// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// holdAndBlockForShare creates a row in pool's database, holds its row lock
// with FOR UPDATE and starts a FOR SHARE statement that blocks behind that
// lock. It returns once pool's database shows the blocked statement. Cleanup
// releases the lock and waits for the blocked statement.
func holdAndBlockForShare(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if _, err := pool.Exec(ctx, "CREATE TABLE lock_probe (id int PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO lock_probe VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(ctx, "SELECT 1 FROM lock_probe WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	waiter, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// The statement blocks until the holder ends; its result is irrelevant.
		_, _ = waiter.Exec(ctx, "SELECT 1 FROM lock_probe WHERE id = 1 FOR SHARE")
	}()
	t.Cleanup(func() {
		// Rolling the holder back releases the lock; canceling the context
		// covers a waiter that has not started yet.
		_ = holder.Rollback(context.Background())
		<-done
		cancel()
		_ = waiter.Rollback(context.Background())
	})
	testenv.Eventually(t, "the FOR SHARE statement waits for the row lock", func() bool {
		n, err := queryForShareLockWaiters(ctx, pool)
		if err != nil {
			t.Fatal(err)
		}
		return n == 1
	})
}

// TestForShareLockWaitersAreScopedToTheDatabase starts a blocked FOR SHARE
// statement in one database of a server and then in a second one, and checks
// that each database counts only its own waiter.
func TestForShareLockWaitersAreScopedToTheDatabase(t *testing.T) {
	a := dbtest.New(t)
	b := dbtest.New(t)
	ctx := context.Background()

	// Only B waits. holdAndBlockForShare returns after B shows its waiter, so
	// the zero count of A is not an artifact of the waiter starting late.
	holdAndBlockForShare(t, b.Pool)
	if n, err := queryForShareLockWaiters(ctx, a.Pool); err != nil || n != 0 {
		t.Fatalf("waiters seen from A while only B waits = %d, err = %v", n, err)
	}

	// Both wait: each database counts exactly its own waiter.
	holdAndBlockForShare(t, a.Pool)
	for name, pool := range map[string]*pgxpool.Pool{"A": a.Pool, "B": b.Pool} {
		if n, err := queryForShareLockWaiters(ctx, pool); err != nil || n != 1 {
			t.Fatalf("waiters seen from %s while both wait = %d, err = %v", name, n, err)
		}
	}
}

// TestForShareLockWaitersReportQueryError checks that a failing activity
// query surfaces its error instead of reading as zero waiters.
func TestForShareLockWaitersReportQueryError(t *testing.T) {
	d := dbtest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n, err := queryForShareLockWaiters(ctx, d.Pool)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled query returned n = %d, and its error does not wrap context.Canceled", n)
	}
}
