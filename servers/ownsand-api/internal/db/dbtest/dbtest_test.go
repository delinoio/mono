// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// TestNewTargetsGeneratedDatabase runs against Postgres with the admin URL
// from the environment, whatever form of database setting it uses.
func TestNewTargetsGeneratedDatabase(t *testing.T) {
	u := NewURL(t)
	ctx := context.Background()
	database, err := db.Open(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	var current string
	if err := database.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(current, "ownsand_test_") {
		t.Fatalf("current_database = %q, want a generated database", current)
	}
	var migrated bool
	if err := database.Pool.QueryRow(ctx, "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if !migrated {
		t.Fatal("generated database has no goose_db_version table")
	}
}
