// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	cleanupFixtureName   = "ownsand_test_0011223344556677"
	cleanupFixtureSecret = "secret-pw"
	cleanupFixtureHost   = "db.internal.example"
)

// recorder is an errorfer that records the failure messages.
type recorder struct {
	messages []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

// fakeConn is an adminConn with a scripted Exec result.
type fakeConn struct {
	execErr  error
	execSQL  []string
	closed   bool
	deadline bool
}

func (c *fakeConn) Exec(ctx context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.execSQL = append(c.execSQL, sql)
	_, c.deadline = ctx.Deadline()
	return pgconn.CommandTag{}, c.execErr
}

func (c *fakeConn) Close(context.Context) error {
	c.closed = true
	return nil
}

// leakyError carries connection details like a pgx error can.
func leakyError() error {
	return errors.New("failed to connect to `host=" + cleanupFixtureHost + " user=admin password=" + cleanupFixtureSecret + "`")
}

func assertOneSafeMessage(t *testing.T, r *recorder, operation string) {
	t.Helper()
	if len(r.messages) != 1 {
		t.Fatalf("got %d failure messages, want 1: %q", len(r.messages), r.messages)
	}
	msg := r.messages[0]
	for _, want := range []string{operation, cleanupFixtureName} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message %q does not contain %q", msg, want)
		}
	}
	for _, leak := range []string{cleanupFixtureSecret, cleanupFixtureHost, "password", "postgres"} {
		if strings.Contains(msg, leak) {
			t.Fatalf("message %q leaks %q", msg, leak)
		}
	}
}

func TestDropDatabaseReportsConnectFailure(t *testing.T) {
	r := &recorder{}
	dropDatabase(r, cleanupFixtureName, func(context.Context) (adminConn, error) {
		return nil, leakyError()
	})
	assertOneSafeMessage(t, r, "connect")
}

func TestDropDatabaseReportsDropFailure(t *testing.T) {
	r := &recorder{}
	conn := &fakeConn{execErr: leakyError()}
	dropDatabase(r, cleanupFixtureName, func(context.Context) (adminConn, error) {
		return conn, nil
	})
	assertOneSafeMessage(t, r, "drop")
	if !conn.closed {
		t.Fatal("admin connection was not closed after a failed drop")
	}
}

func TestDropDatabaseSuccessDropsOnlyTheGeneratedDatabase(t *testing.T) {
	r := &recorder{}
	conn := &fakeConn{}
	dropDatabase(r, cleanupFixtureName, func(context.Context) (adminConn, error) {
		return conn, nil
	})
	if len(r.messages) != 0 {
		t.Fatalf("unexpected failure messages: %q", r.messages)
	}
	want := "DROP DATABASE IF EXISTS " + cleanupFixtureName + " WITH (FORCE)"
	if len(conn.execSQL) != 1 || conn.execSQL[0] != want {
		t.Fatalf("statements = %q, want only %q", conn.execSQL, want)
	}
	if !conn.closed {
		t.Fatal("admin connection was not closed")
	}
	if !conn.deadline {
		t.Fatal("cleanup context has no deadline")
	}
}

// TestNewURLDropsDatabaseOnCleanup runs against Postgres. The generated
// database exists while the subtest runs and is gone once its cleanup ran.
func TestNewURLDropsDatabaseOnCleanup(t *testing.T) {
	admin := URL(t)
	ctx := context.Background()
	var name string
	connect := func(t *testing.T, url string) *pgx.Conn {
		t.Helper()
		cfg, err := pgxpool.ParseConfig(url)
		if err != nil {
			t.Fatal("parse database configuration failed")
		}
		conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
		if err != nil {
			t.Fatal("connect to the database failed")
		}
		t.Cleanup(func() { _ = conn.Close(ctx) })
		return conn
	}
	t.Run("fixture", func(t *testing.T) {
		conn := connect(t, NewURL(t))
		if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
			t.Fatal("read the generated database name failed")
		}
	})
	if !strings.HasPrefix(name, "ownsand_test_") {
		t.Fatalf("generated database name = %q", name)
	}
	conn := connect(t, admin)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatal("query pg_database failed")
	}
	if exists {
		t.Fatalf("database %s still exists after the cleanup", name)
	}
}
