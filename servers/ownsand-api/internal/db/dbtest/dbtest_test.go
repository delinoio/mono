// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestChildURL(t *testing.T) {
	const name = "ownsand_test_0011223344556677"
	cases := []struct {
		label     string
		admin     string
		wantQuery string
	}{
		{"path only", "postgresql://fixture@localhost/admin", "dbname=" + name},
		{"empty path", "postgresql://fixture@localhost", "dbname=" + name},
		{"dbname", "postgresql://fixture@localhost/?dbname=fixture_admin&sslmode=disable", "dbname=fixture_admin&sslmode=disable&dbname=" + name},
		{"database alias", "postgresql://fixture@localhost/?database=fixture_admin&sslmode=disable", "database=fixture_admin&sslmode=disable&dbname=" + name},
		{"both aliases", "postgresql://fixture@localhost/?dbname=x&database=y", "dbname=x&database=y&dbname=" + name},
		{"raw bytes and order", "postgres://fixture@localhost/admin?z=1&a=b%20c+d&dbname=x", "z=1&a=b%20c+d&dbname=x&dbname=" + name},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			got, err := childURL(tc.admin, name)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasSuffix(got, "?"+tc.wantQuery) {
				t.Fatalf("query of %q is not %q", got, tc.wantQuery)
			}
			cfg, err := pgxpool.ParseConfig(got)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.ConnConfig.Database != name {
				t.Fatalf("database = %q, want %q", cfg.ConnConfig.Database, name)
			}
		})
	}
}

func TestChildURLErrorOmitsURL(t *testing.T) {
	_, err := childURL("postgresql://user:secret-pw@local host:5432/%zz", "n")
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "secret-pw") {
		t.Fatalf("error leaks the URL: %v", err)
	}
}

// TestNewTargetsGeneratedDatabase runs against Postgres with the admin URL
// from the environment, whatever form of database setting it uses.
func TestNewTargetsGeneratedDatabase(t *testing.T) {
	u := NewURL(t)
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, u)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var current string
	if err := conn.QueryRow(ctx, "SELECT current_database()").Scan(&current); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(current, "ownsand_test_") {
		t.Fatalf("current_database = %q, want a generated database", current)
	}
	var migrated bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('goose_db_version') IS NOT NULL").Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if !migrated {
		t.Fatal("generated database has no goose_db_version table")
	}
}
