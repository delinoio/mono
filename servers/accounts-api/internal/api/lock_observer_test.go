// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/accounts-api/internal/testenv"
)

const lockObservationTimeout = 15 * time.Second

func observeLockWaiters(pool *pgxpool.Pool, timeout time.Duration, observation, query string,
	ready func(waiting int) (bool, error), args ...any) error {
	waitCtx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// The holder and waiting RPCs can occupy every connection in the pool.
	conn, err := pgx.ConnectConfig(waitCtx, pool.Config().ConnConfig)
	if err != nil {
		return fmt.Errorf("%s: connect to PostgreSQL: %w", observation, err)
	}
	defer func() { _ = conn.Close(waitCtx) }()

	for waitCtx.Err() == nil {
		var waiting int
		if err := conn.QueryRow(waitCtx, query, args...).Scan(&waiting); err != nil {
			return fmt.Errorf("%s: query PostgreSQL: %w", observation, err)
		}
		matched, err := ready(waiting)
		if err != nil {
			return fmt.Errorf("%s: %w", observation, err)
		}
		if matched && waitCtx.Err() == nil {
			return nil
		}
		pauseBetweenPolls(waitCtx)
	}
	return fmt.Errorf("%s: %w", observation, waitCtx.Err())
}

func waitForLockWaiterCount(t *testing.T, pool *pgxpool.Pool, lockName string, n int, query string) {
	t.Helper()
	observation := fmt.Sprintf("observe %s waiters (expected %d)", lockName, n)
	if err := observeLockWaiters(pool, lockObservationTimeout, observation, query, func(waiting int) (bool, error) {
		return waiting == n, nil
	}); err != nil {
		t.Fatal(err)
	}
}

func rollbackLockHolder(t *testing.T, tx pgx.Tx) {
	t.Helper()
	// A failed observation can already have used all of its time.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), lockObservationTimeout)
	defer cancel()
	if err := tx.Rollback(cleanupCtx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("roll back lock holder: %v", err)
	}
}

func TestLockObserverWithPostgres(t *testing.T) {
	t.Parallel()
	env := testenv.New(t)
	const shortTimeout = time.Second
	expectOne := func(waiting int) (bool, error) { return waiting == 1, nil }

	config := env.DB.Pool.Config()
	config.MaxConns = 1
	config.MinConns = 0
	config.MinIdleConns = 0
	// This server-side limit lets the test fail if the observer loses its context.
	config.ConnConfig.RuntimeParams["lock_timeout"] = "5s"
	pool, err := pgxpool.NewWithConfig(ctx(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	t.Run("saturated pool", func(t *testing.T) {
		tx, err := pool.Begin(ctx())
		if err != nil {
			t.Fatal(err)
		}
		defer rollbackLockHolder(t, tx)

		// Confirm the shared pool cannot provide a connection.
		poolCtx, cancel := context.WithTimeout(ctx(), shortTimeout)
		defer cancel()
		var count int
		err = pool.QueryRow(poolCtx, "SELECT 1").Scan(&count)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("saturated pool query error = %v, want deadline exceeded", err)
		}
		t.Logf("shared pool query: %v", err)
		if err := observeLockWaiters(pool, shortTimeout, "observe with a saturated pool", "SELECT 1", expectOne); err != nil {
			t.Fatal(err)
		}
		t.Log("the observer queried PostgreSQL while the holder occupied the only pooled connection")
	})

	t.Run("stalled query", func(t *testing.T) {
		holdKey(t, env, pauseReportKey)
		const observation = "observe the stalled advisory lock query"
		err := observeLockWaiters(pool, shortTimeout, observation,
			"SELECT 1 FROM pg_advisory_xact_lock($1)", expectOne, pauseReportKey)
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), observation+": query PostgreSQL") {
			t.Fatalf("stalled observation error = %v, want a deadline error naming the observation and query", err)
		}
		t.Log(err)
	})

	// The stalled subtest has finished running the fixture's holder cleanup.
	checkCtx, cancel := context.WithTimeout(ctx(), shortTimeout)
	defer cancel()
	var acquired bool
	if err := env.DB.Pool.QueryRow(checkCtx, "SELECT pg_try_advisory_xact_lock($1)", pauseReportKey).Scan(&acquired); err != nil || !acquired {
		t.Fatalf("acquire advisory lock after observer timeout = %t, %v", acquired, err)
	}
	t.Log("fixture cleanup released the advisory lock after the stalled query timed out")
}
