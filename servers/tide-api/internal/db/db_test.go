// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"regexp"
	"testing"

	"github.com/delinoio/mono/servers/tide-api/internal/db"
	"github.com/delinoio/mono/servers/tide-api/internal/db/dbtest"
)

func TestOpenInvalidDatabaseURL(t *testing.T) {
	database, err := db.Open(t.Context(), "postgres://[")
	if database != nil {
		database.Close()
		t.Fatal("Open returned a database for an invalid URL")
	}

	const want = "parse database url: invalid connection string"
	if err == nil || err.Error() != want {
		t.Fatalf("Open error = %v, want %q", err, want)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("Open error wraps the parser error")
	}
}

// TestMigrationNames checks the naming convention of the embedded files.
func TestMigrationNames(t *testing.T) {
	names, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v %v", names, err)
	}
	pattern := regexp.MustCompile(`^[0-9]{5}_[a-z0-9_]+\.sql$`)
	for _, name := range names {
		if !pattern.MatchString(name) {
			t.Errorf("%s does not match <5-digit version>_<name>.sql", name)
		}
	}
}

// TestMigrateTwice checks that a second run on a migrated database applies
// nothing and that the core tables exist.
func TestMigrateTwice(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if err := d.Migrate(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"organizations", "memberships", "invitations", "api_keys", "audit_events", "instance_leases"} {
		var exists bool
		if err := d.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
			t.Errorf("table %s: %v %v", table, exists, err)
		}
	}
}
