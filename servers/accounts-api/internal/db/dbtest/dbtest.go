// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dbtest gives tests a fresh, migrated Postgres database. Tests that
// use it skip when ACCOUNTS_TEST_DATABASE_URL is unset.
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

	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// EnvTestDatabaseURL names a Postgres URL whose user may create databases.
const EnvTestDatabaseURL = "ACCOUNTS_TEST_DATABASE_URL"

// URL returns the admin URL or skips the test.
func URL(t testing.TB) string {
	t.Helper()
	admin := os.Getenv(EnvTestDatabaseURL)
	if admin == "" {
		t.Skipf("%s is not set; skipping a Postgres-backed test", EnvTestDatabaseURL)
	}
	return admin
}

// NewURL creates a fresh database with a unique name, applies every
// migration and returns its URL. The database is dropped when the test ends.
func NewURL(t testing.TB) string {
	t.Helper()
	return newURL(t, 0)
}

// NewURLAtVersion is like NewURL but applies only the migrations up to and
// including version, so an upgrade test can insert data in the old schema and
// then apply the later migrations with db.Migrate.
func NewURLAtVersion(t testing.TB, version int64) string {
	t.Helper()
	return newURL(t, version)
}

// NewEmptyURL creates a fresh database without migrations and returns its
// URL. The database is dropped when the test ends.
func NewEmptyURL(t testing.TB) string {
	t.Helper()
	return newURL(t, -1)
}

func newURL(t testing.TB, version int64) string {
	t.Helper()
	admin := URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to test database: %v", err)
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := "accounts_test_" + hex.EncodeToString(b[:])
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		_ = conn.Close(ctx)
		t.Fatalf("create test database: %v", err)
	}
	_ = conn.Close(ctx)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	child, err := childURL(admin, name)
	if err != nil {
		t.Fatalf("derive test database URL from %s: %v", EnvTestDatabaseURL, err)
	}
	if version < 0 {
		return child
	}
	d, err := db.Open(ctx, child)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer d.Close()
	if err := d.MigrateTo(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil)), version); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return child
}

// childURL returns the admin URL retargeted to the database name. It rewrites
// the path and appends dbname=name as the last query setting, because pgx
// lets the dbname query setting override the path and the last duplicate
// wins. The error never contains the URL.
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
	d, err := db.Open(context.Background(), NewURL(t))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(d.Close)
	return d
}
