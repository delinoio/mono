// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	onExec      func()
	statements  []string
	hasDeadline bool
	closed      bool
}

func (c *fakeAdminConn) Exec(ctx context.Context, sql string, _ ...any) (pgconn.CommandTag, error) {
	c.statements = append(c.statements, sql)
	_, c.hasDeadline = ctx.Deadline()
	if c.onExec != nil {
		c.onExec()
	}
	return pgconn.CommandTag{}, c.execErr
}

func (c *fakeAdminConn) Close(context.Context) error {
	c.closed = true
	return nil
}

type beforeSendError struct{}

func (beforeSendError) Error() string     { return "query was never sent" }
func (beforeSendError) SafeToRetry() bool { return true }

func TestCreateDatabaseCleanup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		created     bool
		collision   bool
		wantCleanup bool
	}{
		{name: "success", created: true, wantCleanup: true},
		{name: "lost reply", err: io.EOF, created: true, wantCleanup: true},
		{
			name:      "duplicate database",
			err:       &pgconn.PgError{SeverityUnlocalized: "ERROR", Code: "42P04"},
			collision: true,
		},
		{
			name: "permission denied",
			err:  &pgconn.PgError{SeverityUnlocalized: "ERROR", Code: "42501"},
		},
		{name: "before send", err: beforeSendError{}},
		{
			name:        "session terminated",
			err:         &pgconn.PgError{SeverityUnlocalized: "FATAL", Code: "57P01"},
			wantCleanup: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			catalog := map[string]bool{"unrelated_database": true}
			if tc.collision {
				catalog[cleanupFixtureName] = true
			}
			createConn := &fakeAdminConn{execErr: tc.err}
			if tc.created {
				createConn.onExec = func() { catalog[cleanupFixtureName] = true }
			}
			dropConn := &fakeAdminConn{onExec: func() { delete(catalog, cleanupFixtureName) }}
			connects := 0
			t.Run("create", func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				err := createDatabase(cleanupFixtureName, func(sql string) error {
					_, err := createConn.Exec(ctx, sql)
					return err
				}, func() {
					t.Cleanup(func() {
						dropDatabase(t, cleanupFixtureName, func(ctx context.Context) (adminConn, error) {
							connects++
							if ctx.Err() != nil {
								t.Errorf("cleanup began with a canceled context: %v", ctx.Err())
							}
							return dropConn, nil
						})
					})
				})
				if !errors.Is(err, tc.err) {
					t.Errorf("create error = %v, want %v", err, tc.err)
				}
				want := []string{"CREATE DATABASE " + cleanupFixtureName}
				if !slices.Equal(createConn.statements, want) {
					t.Errorf("create statements = %q, want %q", createConn.statements, want)
				}
			})
			if tc.wantCleanup {
				want := []string{"DROP DATABASE IF EXISTS " + cleanupFixtureName + " WITH (FORCE)"}
				if connects != 1 || !dropConn.closed || !dropConn.hasDeadline || !slices.Equal(dropConn.statements, want) {
					t.Errorf("cleanup connects = %d, closed = %t, deadline = %t, SQL = %q", connects, dropConn.closed, dropConn.hasDeadline, dropConn.statements)
				}
			} else if connects != 0 {
				t.Errorf("unexpected cleanup connections: %d", connects)
			}
			if !catalog["unrelated_database"] || catalog[cleanupFixtureName] != tc.collision {
				t.Errorf("unexpected final catalog: %v", catalog)
			}
		})
	}
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
