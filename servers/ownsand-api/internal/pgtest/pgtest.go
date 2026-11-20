// SPDX-License-Identifier: FSL-1.1-ALv2

// Package pgtest gives storage tests a fresh, migrated Postgres database.
// Tests that use it are skipped unless OWNSAND_TEST_DATABASE_URL names a
// Postgres server where the test user may create databases, for example:
//
//	docker run --rm -d -e POSTGRES_PASSWORD=test -p 127.0.0.1:55432:5432 postgres:17
//	OWNSAND_TEST_DATABASE_URL=postgres://postgres:test@127.0.0.1:55432/postgres?sslmode=disable go test ./servers/ownsand-api/...
package pgtest

import (
	"context"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// EnvURL names the admin connection string of the test server.
const EnvURL = "OWNSAND_TEST_DATABASE_URL"

// New creates a database, applies the migrations and returns the store. The
// database is dropped when the test ends.
func New(t testing.TB) *store.Store {
	t.Helper()
	admin := os.Getenv(EnvURL)
	if admin == "" {
		t.Skip(EnvURL + " is not set; skipping the Postgres storage test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		t.Fatalf("connect to the test server: %v", err)
	}
	name := "ownsand_test_" + strings.ReplaceAll(datapath.NewID(), "-", "")
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create database: %v", err)
	}
	st, err := store.Open(ctx, withDatabase(admin, name))
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	t.Cleanup(func() {
		st.Close()
		dropCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
		_ = conn.Close(dropCtx)
	})
	if err := store.Migrate(ctx, st.Pool, slog.New(slog.NewTextHandler(io.Discard, nil))); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// withDatabase replaces the database path of a postgres:// URL.
func withDatabase(url, name string) string {
	scheme, rest, ok := strings.Cut(url, "://")
	if !ok {
		return url
	}
	query := ""
	if i := strings.IndexByte(rest, '?'); i >= 0 {
		rest, query = rest[:i], rest[i:]
	}
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		rest = rest[:i]
	}
	return scheme + "://" + rest + "/" + name + query
}
