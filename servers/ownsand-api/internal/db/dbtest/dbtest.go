// SPDX-License-Identifier: FSL-1.1-ALv2

// Package dbtest gives tests a fresh, migrated Postgres database. Tests that
// use it skip when OWNSAND_TEST_DATABASE_URL is unset.
package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"io"
	"log/slog"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

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
	admin := URL(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, admin)
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
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		conn, err := pgx.Connect(ctx, admin)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close(ctx) }()
		_, _ = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	u, err := url.Parse(admin)
	if err != nil {
		t.Fatalf("parse %s: %v", EnvTestDatabaseURL, err)
	}
	u.Path = "/" + name
	d, err := db.Open(ctx, u.String())
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	defer d.Close()
	if err := d.Migrate(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate test database: %v", err)
	}
	return u.String()
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
