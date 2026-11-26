// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dbtest gives tests a fresh, migrated Postgres database. Tests that
// use it skip when SANDRUN_TEST_DATABASE_URL is unset.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

// EnvTestDatabaseURL names a Postgres URL whose user may create databases.
const EnvTestDatabaseURL = "SANDRUN_TEST_DATABASE_URL"

// URL returns the admin URL or skips the test.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvTestDatabaseURL)
	if admin == "" {
		t.Skipf("%s is not set; skipping a Postgres-backed test", EnvTestDatabaseURL)
	}
	return admin
}

// NewURL creates a fresh database, applies the migrations and returns its
// URL. The database is dropped when the test ends.
func NewURL(t testing.TB) string {
	t.Helper()
	return newURL(t, 0)
}

// NewURLAtVersion is like NewURL but applies only the migrations up to and
// including maxVersion, so an upgrade test can insert legacy data and then
// apply the later migrations with db.MigrateUpTo or db.Migrate.
func NewURLAtVersion(t testing.TB, maxVersion int64) string {
	t.Helper()
	return newURL(t, maxVersion)
}

func newURL(t testing.TB, maxVersion int64) string {
	t.Helper()
	admin := URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	config, err := pgx.ParseConfig(admin)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	// This connection runs one statement. Close on a server error so pgx
	// returns the error even if the connection fails before ReadyForQuery.
	config.OnPgError = func(*pgconn.PgConn, *pgconn.PgError) bool { return false }
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := "sandrun_test_" + hex.EncodeToString(b[:])
	createErr := createDatabase(name, func(sql string) error {
		// Read the response directly. pgx.Conn.Exec can replace a read EOF
		// with a "conn closed" error that SafeToRetry treats as before-send.
		return conn.PgConn().Exec(ctx, sql).Close()
	}, func() {
		t.Cleanup(func() {
			dropDatabase(t, name, func(ctx context.Context) (adminConn, error) {
				return pgx.Connect(ctx, admin)
			})
		})
	})
	_ = conn.Close(ctx)
	if createErr != nil {
		t.Fatalf("create test database: %v", createErr)
	}
	child, err := childURL(admin, name)
	if err != nil {
		t.Fatalf("derive test database URL from %s: %v", EnvTestDatabaseURL, err)
	}
	d, err := db.Open(ctx, child)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer d.Close()
	if err := d.MigrateUpTo(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)), maxVersion); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return child
}

func createDatabase(name string, exec func(string) error, registerCleanup func()) error {
	err := exec("CREATE DATABASE " + name)
	if pgconn.SafeToRetry(err) {
		return err
	}

	// A statement error, including a duplicate name, confirms that this
	// create failed. A session or transport failure may hide a commit.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.SeverityUnlocalized == "ERROR" {
		return err
	}
	registerCleanup()
	return err
}

type cleanupReporter interface {
	Helper()
	Errorf(format string, args ...any)
}

type adminConn interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Close(ctx context.Context) error
}

func dropDatabase(t cleanupReporter, name string, connect func(context.Context) (adminConn, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Driver errors can contain connection details.
	conn, err := connect(ctx)
	if err != nil {
		t.Errorf("dbtest: connect to drop test database %s failed", name)
		return
	}
	defer func() { _ = conn.Close(ctx) }()

	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
		t.Errorf("dbtest: drop test database %s failed", name)
	}
}

// childURL returns the admin URL retargeted to the database name. It rewrites
// the path for path-only URLs and appends dbname=name as the last query
// setting, because pgx lets the dbname and database query settings override
// the path and the last duplicate wins. The rest of the query keeps its raw
// bytes and order. The error never contains the URL.
func childURL(admin, name string) (string, error) {
	u, err := url.Parse(admin)
	if err != nil {
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return "", uerr.Err
		}
		return "", errors.New("invalid URL")
	}
	u.Path = "/" + name
	u.RawPath = ""
	if u.RawQuery == "" {
		u.RawQuery = "dbname=" + name
	} else {
		u.RawQuery += "&dbname=" + name
	}
	return u.String(), nil
}

// New returns an open pool on a fresh, migrated database.
func New(t testing.TB) *db.DB {
	t.Helper()
	u := NewURL(t)
	d, err := db.Open(context.Background(), u)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}
