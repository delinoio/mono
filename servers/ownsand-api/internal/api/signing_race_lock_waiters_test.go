// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
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
	t.Cleanup(cancel)
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
	var waiter pgx.Tx
	var done chan struct{}
	t.Cleanup(func() {
		// Release the row lock and let the waiter finish using its transaction.
		_ = holder.Rollback(context.Background())
		if done != nil {
			<-done
		}
		if waiter != nil {
			_ = waiter.Rollback(context.Background())
		}
	})
	if _, err := holder.Exec(ctx, "SELECT 1 FROM lock_probe WHERE id = 1 FOR UPDATE"); err != nil {
		t.Fatal(err)
	}
	waiter, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	done = make(chan struct{})
	go func() {
		defer close(done)
		// The statement blocks until the holder ends; its result is irrelevant.
		_, _ = waiter.Exec(ctx, "SELECT 1 FROM lock_probe WHERE id = 1 FOR SHARE")
	}()
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

const (
	signingLockFailureEnv = "OWNSAND_TEST_SIGNING_LOCK_FAILURE"
	waiterBeginError      = "injected waiter begin error"
	signingLockPoolClosed = "signing lock fixture pool closed"
)

func TestForShareLockWaitersCleanUpAfterSetupError(t *testing.T) {
	if failure := os.Getenv(signingLockFailureEnv); failure != "" {
		failSigningLockFixtureSetup(t, failure)
		return
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name      string
		wantError string
	}{
		{name: "holder_statement", wantError: "SQLSTATE 25006"},
		{name: "waiter_begin", wantError: waiterBeginError},
	} {
		t.Run(tt.name, func(t *testing.T) {
			// Own the database in the parent so it can be dropped if the child hangs.
			databaseURL := dbtest.NewURL(t)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable,
				"-test.run=^TestForShareLockWaitersCleanUpAfterSetupError$", "-test.v", "-test.timeout=30s")
			cmd.Env = append(cmd.Environ(), signingLockFailureEnv+"="+tt.name, dbtest.EnvTestDatabaseURL+"="+databaseURL)
			output, err := cmd.CombinedOutput()
			if ctx.Err() != nil {
				t.Fatalf("fixture subprocess timed out: %s", output)
			}
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
				t.Fatalf("fixture subprocess should fail with exit code 1, got %v: %s", err, output)
			}
			for _, want := range []string{tt.wantError, signingLockPoolClosed} {
				if !strings.Contains(string(output), want) {
					t.Fatalf("fixture subprocess output is missing %q: %s", want, output)
				}
			}
		})
	}
}

func failSigningLockFixtureSetup(t *testing.T, failure string) {
	t.Helper()
	config, err := pgxpool.ParseConfig(dbtest.URL(t))
	if err != nil {
		t.Fatal("parse signing lock fixture database configuration")
	}
	config.MaxConns = 3
	config.MinConns = 0
	config.MinIdleConns = 0
	var checkouts int
	config.PrepareConn = func(ctx context.Context, conn *pgx.Conn) (bool, error) {
		checkouts++
		// The first two checkouts create the table and insert the row.
		if checkouts == 3 && failure == "holder_statement" {
			_, err := conn.Exec(ctx, "SET default_transaction_read_only = on")
			return err == nil, err
		}
		if checkouts == 4 && failure == "waiter_begin" {
			return false, errors.New(waiterBeginError)
		}
		return true, nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		t.Log(signingLockPoolClosed)
	})

	holdAndBlockForShare(t, pool)
}
