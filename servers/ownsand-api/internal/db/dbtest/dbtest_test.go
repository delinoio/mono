// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/delinoio/mono/servers/internal/dbtesturl"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

func TestWithTestPoolDefault(t *testing.T) {
	const admin = "postgresql://fixture@localhost/admin"
	const generated = "ownsand_test_generated"
	tests := []struct {
		name        string
		adminURL    string
		wantAppend  bool
		wantMax     int32
		wantAppName string
		wantError   bool
	}{
		{name: "plain URL", adminURL: admin, wantAppend: true, wantMax: 32},
		{
			name:        "option name in an unrelated value",
			adminURL:    admin + "?application_name=pool_max_conns",
			wantAppend:  true,
			wantMax:     32,
			wantAppName: "pool_max_conns",
		},
		{
			name:        "query order and session value",
			adminURL:    admin + "?dbname=other&application_name=a;b+c%20d&application_name=last;+value&database=other",
			wantAppend:  true,
			wantMax:     32,
			wantAppName: "last;+value",
		},
		{name: "literal key", adminURL: admin + "?pool_max_conns=12", wantMax: 12},
		{name: "encoded key", adminURL: admin + "?pool%5Fmax_conns=12", wantMax: 12},
		{name: "key with spaces", adminURL: admin + "? pool%5Fmax_conns =12", wantMax: 12},
		{name: "repeated key", adminURL: admin + "?pool_max_conns=12&pool%5Fmax_conns=16", wantMax: 16},
		{name: "invalid overridden value", adminURL: admin + "?pool%5Fmax_conns=invalid&pool_max_conns=16", wantMax: 16},
		{name: "invalid final value", adminURL: admin + "?pool%5Fmax_conns=12&pool%5Fmax_conns=invalid", wantError: true},
		{name: "empty value", adminURL: admin + "?pool%5Fmax_conns=", wantError: true},
		{
			name:     "encoded socket authority",
			adminURL: "postgresql://fixture@%2Fvar%2Frun%2Fpostgresql/admin?pool%5Fmax_conns=12",
			wantMax:  12,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			child, err := dbtesturl.Child(tt.adminURL, generated)
			if err != nil {
				t.Fatal(err)
			}
			wantURL := child
			if tt.wantAppend {
				wantURL += "&pool_max_conns=32"
			}
			gotURL := withTestPoolDefault(child)
			if gotURL != wantURL {
				t.Fatalf("pool URL = %q, want %q", gotURL, wantURL)
			}
			cfg, err := pgxpool.ParseConfig(gotURL)
			if tt.wantError {
				if err == nil || !strings.Contains(err.Error(), "pool_max_conns") {
					t.Fatalf("parse pool URL error = %v, want a pool_max_conns error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.MaxConns != tt.wantMax {
				t.Errorf("MaxConns = %d, want %d", cfg.MaxConns, tt.wantMax)
			}
			if cfg.ConnConfig.Database != generated {
				t.Errorf("database = %q, want %q", cfg.ConnConfig.Database, generated)
			}
			if got := cfg.ConnConfig.RuntimeParams["application_name"]; got != tt.wantAppName {
				t.Errorf("application_name = %q, want %q", got, tt.wantAppName)
			}
		})
	}
}

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
