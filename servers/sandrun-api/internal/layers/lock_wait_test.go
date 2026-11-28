// SPDX-License-Identifier: FSL-1.1-ALv2

package layers_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/sandrun-api/internal/testenv"
)

func TestLockWaitObserverMatchesHolderAndStatement(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	holder := holdTx(t, env, "SELECT pg_advisory_xact_lock(101), pg_advisory_xact_lock(102)")
	other := holdTx(t, env, "SELECT pg_advisory_xact_lock(103)")

	// Use a separate pool so both decoys can stay blocked during observation.
	config := env.DB.Pool.Config()
	config.MaxConns = 3
	waiters, err := pgxpool.NewWithConfig(t.Context(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(waiters.Close)
	start := func(query string, key int64) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := waiters.Exec(t.Context(), query, key)
			done <- err
		}()
		return done
	}

	const targetSQL = "SELECT pg_advisory_xact_lock($1)"
	const unrelatedSQL = "SELECT 1 FROM pg_advisory_xact_lock($1)"
	wrongHolder := start(targetSQL, 103)
	waitForLockWaiter(t, env, lockWait{blockerPID: other.pid, query: targetSQL})
	wrongStatement := start(unrelatedSQL, 102)
	waitForLockWaiter(t, env, lockWait{blockerPID: holder.pid, query: unrelatedSQL})

	wait := lockWait{blockerPID: holder.pid, query: targetSQL}
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if pid, err := observeLockWaiter(ctx, env.DB.Pool, wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("observe with only unrelated waiters = (%d, %v), want deadline exceeded", pid, err)
	}

	target := start(targetSQL, 101)
	waitForLockWaiter(t, env, wait)
	holder.commit()
	other.commit()
	for _, done := range []<-chan error{target, wrongStatement, wrongHolder} {
		if err := <-done; err != nil {
			t.Fatalf("waiter after holder committed: %v", err)
		}
	}
}

func TestLockWaitObserverDeadline(t *testing.T) {
	t.Parallel()
	t.Run("pool acquisition", func(t *testing.T) {
		env := testenv.New(t)
		config := env.DB.Pool.Config()
		config.MaxConns = 1
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()

		t.Run("reserved connection", func(t *testing.T) {
			holder := holdTx(t, env, "SELECT pg_advisory_xact_lock(101)")
			conn, err := pool.Acquire(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Release()

			assertLockWaitDeadline(t, pool, holder.pid)
			if acquired := pool.Stat().AcquiredConns(); acquired != 1 {
				t.Fatalf("reserved connections = %d, want 1", acquired)
			}
		})

		var released bool
		if err := env.DB.Pool.QueryRow(t.Context(), "SELECT pg_try_advisory_xact_lock(101)").Scan(&released); err != nil || !released {
			t.Fatalf("fixture lock available after cleanup = %t, %v", released, err)
		}
	})

	t.Run("running query", func(t *testing.T) {
		env := testenv.New(t)
		// Shadow the system view only for the observer. The test user can
		// lock this table without needing privileges on the system view.
		_, err := env.DB.Pool.Exec(t.Context(), `
			CREATE SCHEMA observer_fixture;
			CREATE TABLE observer_fixture.pg_stat_activity (datname name, pid integer, wait_event_type text, query text)`)
		if err != nil {
			t.Fatal(err)
		}
		config := env.DB.Pool.Config()
		config.ConnConfig.RuntimeParams["search_path"] = "observer_fixture,pg_catalog"
		pool, err := pgxpool.NewWithConfig(t.Context(), config)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(pool.Close)
		if err := pool.Ping(t.Context()); err != nil {
			t.Fatal(err)
		}
		holder := holdTx(t, env, "LOCK TABLE observer_fixture.pg_stat_activity IN ACCESS EXCLUSIVE MODE")
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		result := make(chan error, 1)
		go func() {
			_, err := observeLockWaiter(ctx, pool, lockWait{blockerPID: holder.pid, query: "SELECT pg_advisory_xact_lock($1)"})
			result <- err
		}()
		waitForLockWaiter(t, env, lockWait{blockerPID: holder.pid, query: "SELECT pid FROM pg_stat_activity"})
		requireLockWaitDeadline(t, result)
		holder.commit()
	})
}

func assertLockWaitDeadline(t *testing.T, pool *pgxpool.Pool, blockerPID int32) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := observeLockWaiter(ctx, pool, lockWait{blockerPID: blockerPID, query: "SELECT pg_advisory_xact_lock($1)"})
		result <- err
	}()
	requireLockWaitDeadline(t, result)
}

func requireLockWaitDeadline(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("observe with blocked pool or query = %v, want deadline exceeded", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("lock observation did not return after its deadline")
	}
}
