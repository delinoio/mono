// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const cleanupFixtureName = "tide_test_0011223344556677"

var cleanupDriverError = errors.New("driver error for postgres://fixture:fake-password@db.example.test/postgres")

type cleanupRecorder struct {
	messages []string
}

func (*cleanupRecorder) Helper() {}

func (r *cleanupRecorder) Errorf(format string, args ...any) {
	r.messages = append(r.messages, fmt.Sprintf(format, args...))
}

type fakeAdminConn struct {
	execErr     error
	statements  []string
	hasDeadline bool
	closed      bool
}

func (c *fakeAdminConn) Exec(ctx context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.statements = append(c.statements, sql)
	_, c.hasDeadline = ctx.Deadline()
	return pgconn.CommandTag{}, c.execErr
}

func (c *fakeAdminConn) Close(context.Context) error {
	c.closed = true
	return nil
}

func TestDropDatabaseReportsConnectFailure(t *testing.T) {
	reporter := &cleanupRecorder{}
	dropDatabase(reporter, cleanupFixtureName, func(context.Context) (adminConn, error) {
		return nil, cleanupDriverError
	})

	want := []string{"dbtest: connect to drop test database " + cleanupFixtureName + " failed"}
	if !slices.Equal(reporter.messages, want) {
		t.Fatalf("cleanup messages = %q, want %q", reporter.messages, want)
	}
}

func TestDropDatabaseReportsDropFailure(t *testing.T) {
	reporter := &cleanupRecorder{}
	conn := &fakeAdminConn{execErr: cleanupDriverError}
	dropDatabase(reporter, cleanupFixtureName, func(context.Context) (adminConn, error) {
		return conn, nil
	})

	want := []string{"dbtest: drop test database " + cleanupFixtureName + " failed"}
	if !slices.Equal(reporter.messages, want) {
		t.Fatalf("cleanup messages = %q, want %q", reporter.messages, want)
	}
	if !conn.closed {
		t.Fatal("admin connection stayed open after the drop failed")
	}
}

func TestDropDatabaseSuccessDropsOnlyTheGeneratedDatabase(t *testing.T) {
	reporter := &cleanupRecorder{}
	conn := &fakeAdminConn{}
	dropDatabase(reporter, cleanupFixtureName, func(context.Context) (adminConn, error) {
		return conn, nil
	})

	if len(reporter.messages) != 0 {
		t.Fatalf("unexpected cleanup messages: %q", reporter.messages)
	}
	want := []string{"DROP DATABASE IF EXISTS " + cleanupFixtureName + " WITH (FORCE)"}
	if !slices.Equal(conn.statements, want) {
		t.Fatalf("cleanup statements = %q, want %q", conn.statements, want)
	}
	if !conn.closed {
		t.Fatal("admin connection stayed open after the drop")
	}
	if !conn.hasDeadline {
		t.Fatal("cleanup context has no deadline")
	}
}

func TestNewURLDropsDatabaseOnCleanup(t *testing.T) {
	admin := URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	t.Cleanup(cancel)

	connect := func(t *testing.T, databaseURL string) *pgx.Conn {
		t.Helper()
		conn, err := pgx.Connect(ctx, databaseURL)
		if err != nil {
			t.Fatal("connect to the database failed")
		}
		t.Cleanup(func() { _ = conn.Close(ctx) })
		return conn
	}

	var name string
	if !t.Run("fixture", func(t *testing.T) {
		conn := connect(t, NewURL(t))
		if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
			t.Fatal("read the generated database name failed")
		}
	}) {
		return
	}
	if !strings.HasPrefix(name, "tide_test_") {
		t.Fatalf("unexpected generated database name %q", name)
	}

	conn := connect(t, admin)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatal("query pg_database failed")
	}
	if exists {
		t.Fatalf("database %s still exists after cleanup", name)
	}
}
