// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dbtest gives tests a fresh, migrated Postgres database. Tests that
// use it skip when OWNSAND_TEST_DATABASE_URL is unset.
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
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// EnvTestDatabaseURL names a Postgres URL whose user may create databases.
const EnvTestDatabaseURL = "OWNSAND_TEST_DATABASE_URL"

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
	// Pool options configure the generated database's pool. They must not
	// reach Postgres as session parameters on the admin connections.
	adminConfig, err := pgxpool.ParseConfig(admin)
	if err != nil {
		t.Fatalf("parse %s: invalid database configuration", EnvTestDatabaseURL)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, adminConfig.ConnConfig)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := "ownsand_test_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	_ = conn.Close(ctx)
	t.Cleanup(func() {
		dropDatabase(t, name, func(ctx context.Context) (adminConn, error) {
			return pgx.ConnectConfig(ctx, adminConfig.ConnConfig)
		})
	})
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

// cleanupTimeout bounds the whole cleanup of one generated database.
const cleanupTimeout = 30 * time.Second

// errorfer is the part of testing.TB that dropDatabase reports failures to.
type errorfer interface {
	Helper()
	Errorf(format string, args ...any)
}

// adminConn is the part of *pgx.Conn that dropDatabase uses.
type adminConn interface {
	Exec(ctx context.Context, sql string, arguments ...any) (pgconn.CommandTag, error)
	Close(ctx context.Context) error
}

// dropDatabase drops the generated database name through a fresh admin
// connection and marks the test failed when it cannot connect or cannot drop
// the database, so a leaked database is not silent. The messages name the
// failed operation and the database but never the connection error, because
// a pgx error can contain the host, the user or the connection URL.
func dropDatabase(t errorfer, name string, connect func(ctx context.Context) (adminConn, error)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), cleanupTimeout)
	defer cancel()
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

// testPoolMaxConns is the pool size of the connections that New opens.
const testPoolMaxConns = "32"

// New returns an open pool on a fresh, migrated database.
func New(t testing.TB) *db.DB {
	t.Helper()
	u := NewURL(t)
	// The API serializes pauses and resumes of one sandbox with advisory
	// locks that each hold a pooled connection, and allows half of the pool
	// to hold them at once. pgx sizes the default pool by the CPU count, so a
	// small CI runner would otherwise run fewer than the 8 commands that the
	// billing worker keeps in flight.
	if !strings.Contains(u, "pool_max_conns") {
		u += "&pool_max_conns=" + testPoolMaxConns
	}
	d, err := db.Open(context.Background(), u)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}
