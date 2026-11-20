// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"testing"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestMigrateIsIdempotent applies the migrations to a migrated database
// again and checks that every embedded migration is recorded once.
func TestMigrateIsIdempotent(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&n); err != nil || n == 0 {
		t.Fatalf("schema_migrations rows = %d, %v", n, err)
	}
	// A changed migration is refused.
	if _, err := d.Pool.Exec(ctx, "UPDATE schema_migrations SET sha256 = '\\x00' WHERE version = 1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, testenv.Discard()); err == nil {
		t.Fatal("a changed migration was accepted")
	}
}
