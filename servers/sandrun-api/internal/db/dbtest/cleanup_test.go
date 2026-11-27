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

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

const cleanupFixtureName = "sandrun_test_0011223344556677"

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
	t.Run("configured URL", func(t *testing.T) {
		checkDatabaseLifecycle(t, admin, false)
	})
	t.Run("pool options", func(t *testing.T) {
		separator := "?"
		if strings.Contains(admin, "?") {
			separator = "&"
		}
		const options = "pool_max_conns=4&application_name=sandrun-dbtest"
		checkDatabaseLifecycle(t, admin+separator+options, true)
	})
}

func checkDatabaseLifecycle(t *testing.T, admin string, checkPoolOptions bool) {
	t.Helper()
	t.Setenv(EnvTestDatabaseURL, admin)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	var name string
	if !t.Run("fixture", func(t *testing.T) {
		d, err := db.Open(ctx, NewURL(t))
		if err != nil {
			t.Fatal("connect to the child database failed")
		}
		defer d.Close()
		var migrated bool
		if err := d.Pool.QueryRow(ctx, "SELECT current_database(), to_regclass('organizations') IS NOT NULL").Scan(&name, &migrated); err != nil {
			t.Fatalf("read the child database name and schema: %v", err)
		}
		if !migrated {
			t.Fatal("child database has no organizations table")
		}
		if checkPoolOptions {
			if maxConns := d.Pool.Config().MaxConns; maxConns != 4 {
				t.Errorf("child pool MaxConns = %d, want 4", maxConns)
			}
			var applicationName string
			if err := d.Pool.QueryRow(ctx, "SHOW application_name").Scan(&applicationName); err != nil {
				t.Fatalf("show application_name: %v", err)
			}
			if applicationName != "sandrun-dbtest" {
				t.Errorf("application_name = %q, want sandrun-dbtest", applicationName)
			}
			t.Logf("child pool MaxConns = %d, application_name = %q", d.Pool.Config().MaxConns, applicationName)
		}
	}) {
		return
	}
	if !strings.HasPrefix(name, "sandrun_test_") {
		t.Fatalf("unexpected generated database name %q", name)
	}

	config, err := parseAdminConfig(admin)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal("connect to the admin database failed")
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatal("query pg_database failed")
	}
	if exists {
		t.Fatalf("database %s still exists after cleanup", name)
	}
	t.Logf("migrated database %s is absent from pg_database after cleanup", name)
}

func TestParseAdminConfigRejectsInvalidPoolOption(t *testing.T) {
	admin := "postgres://fixture:fake-password@db.example.test/postgres?pool_max_conns=invalid"
	_, err := parseAdminConfig(admin)
	const want = "invalid PostgreSQL configuration in " + EnvTestDatabaseURL
	if err == nil || err.Error() != want {
		t.Fatalf("configuration error = %v, want %q", err, want)
	}
}
