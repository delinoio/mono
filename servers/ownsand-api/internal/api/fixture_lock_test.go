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

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

const (
	fixtureLockTimeout        = 10 * time.Second
	fixtureLockCleanupTimeout = 5 * time.Second
)

// holdFixtureLock leaves the application pool available to the RPCs that
// participate in the test schedule.
func holdFixtureLock(t *testing.T, pool *pgxpool.Pool, query string, args ...any) pgx.Tx {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), fixtureLockTimeout)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatal("connect fixture lock holder")
	}
	t.Cleanup(func() { closeFixtureLockConn(t, conn) })

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal("begin fixture lock transaction")
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), fixtureLockCleanupTimeout)
		defer cancel()
		if err := tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error("roll back fixture lock transaction during cleanup")
		}
	})

	if _, err := tx.Exec(ctx, query, args...); err != nil {
		t.Fatalf("acquire fixture lock with %q", query)
	}
	return tx
}

func finishFixtureLock(t *testing.T, action string, finish func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureLockCleanupTimeout)
	defer cancel()
	if err := finish(ctx); err != nil {
		t.Fatalf("%s fixture lock transaction", action)
	}
}

func closeFixtureLockConn(t *testing.T, conn *pgx.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureLockCleanupTimeout)
	defer cancel()
	if err := conn.Close(ctx); err != nil {
		t.Error("close dedicated fixture lock connection")
	}
}

// waitForLockWaiter matches statements that contain every fragment.
func waitForLockWaiter(t *testing.T, env *testenv.Env, fragments ...string) {
	t.Helper()
	const query = `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()
			AND wait_event_type = 'Lock' AND query LIKE ALL($1::text[])`
	patterns := make([]string, len(fragments))
	for i, fragment := range fragments {
		patterns[i] = "%" + fragment + "%"
	}
	waitForFixtureLockCount(t, env, 1, fmt.Sprintf("on a statement containing %q", fragments), query, patterns)
}

// waitForOrganizationLockWaiters matches RPCs blocked between their
// authorization pre-check and their authoritative owner check.
func waitForOrganizationLockWaiters(t *testing.T, env *testenv.Env, want int) {
	t.Helper()
	const query = `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'
			AND query LIKE '%FROM organizations WHERE organization_id%FOR UPDATE%'`
	waitForFixtureLockCount(t, env, want, "on the organization row", query)
}

func waitForDatabaseLockWaiters(t *testing.T, env *testenv.Env, want int) {
	t.Helper()
	const query = `SELECT count(*) FROM pg_catalog.pg_stat_activity
		WHERE datname = current_database() AND wait_event_type = 'Lock'`
	waitForFixtureLockCount(t, env, want, "in the test database", query)
}

func waitForFixtureLockCount(t *testing.T, env *testenv.Env, want int, location, query string, args ...any) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), fixtureLockTimeout)
	defer cancel()
	expected := fmt.Sprintf("at least %d lock waiter(s) %s", want, location)
	if err := pollFixtureLockWait(ctx, t, env.DB.Pool, expected, func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		var waiting int
		err := conn.QueryRow(ctx, query, args...).Scan(&waiting)
		return waiting >= want, err
	}); err != nil {
		t.Fatal(err)
	}
}

// pollFixtureLockWait uses the same deadline for the connection, queries,
// results and polling. A separate connection can observe a full application pool.
func pollFixtureLockWait(ctx context.Context, t *testing.T, pool *pgxpool.Pool, expected string, observe func(context.Context, *pgx.Conn) (bool, error)) error {
	t.Helper()
	conn, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		// Connection errors can contain credentials.
		if ctx.Err() != nil {
			return fmt.Errorf("connect observer for %s: %w", expected, ctx.Err())
		}
		return fmt.Errorf("connect observer for %s failed", expected)
	}
	defer closeFixtureLockConn(t, conn)

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		waiting, err := observe(ctx, conn)
		if ctx.Err() != nil {
			break
		}
		if err != nil {
			return fmt.Errorf("query observer for %s failed", expected)
		}
		if waiting {
			return nil
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return fmt.Errorf("observe %s: %w", expected, ctx.Err())
}

func TestFixtureLockObservationWithFullPool(t *testing.T) {
	t.Parallel()
	cfg, err := pgxpool.ParseConfig(dbtest.NewURL(t))
	if err != nil {
		t.Fatal("parse generated fixture database configuration")
	}
	cfg.MaxConns = 2
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	ctx, cancel := context.WithTimeout(t.Context(), fixtureLockTimeout)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal("open two-connection fixture pool")
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, "CREATE TABLE fixture_lock_probe AS SELECT 1 AS id"); err != nil {
		t.Fatal(err)
	}
	for range cfg.MaxConns {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Release()
	}

	var observerPID uint32
	if err := pollFixtureLockWait(ctx, t, pool, "a read from the generated database", func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		observerPID = conn.PgConn().PID()
		var correctDatabase bool
		err := conn.QueryRow(ctx, "SELECT current_database() = $1", cfg.ConnConfig.Database).Scan(&correctDatabase)
		return correctDatabase, err
	}); err != nil {
		t.Fatal(err)
	}

	const lockRow = "SELECT id FROM fixture_lock_probe WHERE id = 1 FOR UPDATE"
	var holderPID, stalledObserverPID uint32
	if !t.Run("query deadline", func(t *testing.T) {
		holder := holdFixtureLock(t, pool, lockRow)
		holderPID = holder.Conn().PgConn().PID()
		observeCtx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		const expected = "a waiter on the fixture probe row"
		start := time.Now()
		err := pollFixtureLockWait(observeCtx, t, pool, expected, func(ctx context.Context, conn *pgx.Conn) (bool, error) {
			stalledObserverPID = conn.PgConn().PID()
			var id int
			err := conn.QueryRow(ctx, lockRow).Scan(&id)
			return id == 1, err
		})
		elapsed := time.Since(start)
		if stalledObserverPID == 0 || !errors.Is(err, context.DeadlineExceeded) ||
			!strings.Contains(err.Error(), expected) || elapsed > 3*time.Second {
			t.Fatalf("stalled query returned after %v with %v, want deadline for %s", elapsed, err, expected)
		}
		t.Logf("stalled observer returned after %v: %v", elapsed, err)
		// Leave the transaction open to exercise fixture cleanup.
	}) {
		t.FailNow()
	}

	// A fresh session can take the same lock while both pooled sessions are
	// still checked out. All dedicated sessions must leave PostgreSQL.
	checker, err := pgx.ConnectConfig(ctx, pool.Config().ConnConfig)
	if err != nil {
		t.Fatal("connect fixture cleanup checker")
	}
	defer closeFixtureLockConn(t, checker)
	var id int
	if err := checker.QueryRow(ctx, lockRow+" NOWAIT").Scan(&id); err != nil || id != 1 {
		t.Fatalf("lock probe row after cleanup: id = %d, error = %v", id, err)
	}
	testenv.Eventually(t, "dedicated holder and observers to disconnect", func() bool {
		var count int
		if err := checker.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE datname = current_database() AND pid IN ($1, $2, $3)`,
			int32(holderPID), int32(observerPID), int32(stalledObserverPID)).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count == 0
	})
}
