// SPDX-License-Identifier: FSL-1.1-ALv2

package dbtest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// recordingTB captures Errorf calls so a test can assert on what cleanup
// reports without failing itself.
type recordingTB struct {
	testing.TB
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Helper() {}

func TestCleanupDatabaseReportsAdminConnectionFailure(t *testing.T) {
	const password = "s3cr3t-pa55"
	// Port 1 on loopback refuses connections, so the connect step fails.
	admin := "postgres://admin:" + password + "@127.0.0.1:1/postgres?sslmode=disable&connect_timeout=2"
	rec := &recordingTB{TB: t}
	cleanupDatabase(rec, admin, "accounts_test_abc")
	if len(rec.errors) != 1 {
		t.Fatalf("cleanup reported %d errors, want 1: %q", len(rec.errors), rec.errors)
	}
	msg := rec.errors[0]
	for _, want := range []string{"connect to test database server", "accounts_test_abc"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	for _, leaked := range []string{password, admin, "postgres://"} {
		if strings.Contains(msg, leaked) {
			t.Errorf("error %q leaks %q", msg, leaked)
		}
	}
}

func TestRedact(t *testing.T) {
	admin := "postgres://u:hunter2@h:5432/postgres"
	got := redact(errors.New("dial "+admin+" failed: password hunter2 rejected"), admin)
	for _, leaked := range []string{"hunter2", admin} {
		if strings.Contains(got, leaked) {
			t.Errorf("redact left %q in %q", leaked, got)
		}
	}
}

func TestConnectAdminRejectsMalformedPoolOption(t *testing.T) {
	const password = "fixture-password"
	const admin = "postgres://admin:" + password + "@127.0.0.1:1/postgres?pool_max_conns=invalid"
	conn, err := connectAdmin(context.Background(), admin, nil)
	if err == nil {
		_ = conn.Close(context.Background())
		t.Fatal("connectAdmin accepted a malformed pool option")
	}
	msg := err.Error()
	if !strings.Contains(msg, "invalid database configuration") {
		t.Error("connectAdmin did not report a configuration error")
	}
	for _, leaked := range []string{password, admin, "postgres://"} {
		if strings.Contains(msg, leaked) {
			t.Error("connectAdmin error includes connection credentials or the URL")
			break
		}
	}
}

func TestCleanupDatabaseReportsDropFailure(t *testing.T) {
	admin := URL(t)
	// A name that is not a valid identifier makes DROP DATABASE fail while
	// the admin connection itself succeeds.
	const name = "accounts_test_bad name;"
	rec := &recordingTB{TB: t}
	cleanupDatabase(rec, admin, name)
	if len(rec.errors) != 1 {
		t.Fatalf("cleanup reported %d errors, want 1: %q", len(rec.errors), rec.errors)
	}
	msg := rec.errors[0]
	for _, want := range []string{"drop test database", name} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, admin) {
		t.Errorf("error %q leaks the admin URL", msg)
	}
}

func TestCleanupDropsGeneratedDatabase(t *testing.T) {
	admin := URL(t)
	const applicationName = "accounts-dbtest"
	separator := "?"
	if strings.Contains(admin, "?") {
		separator = "&"
	}
	for _, tc := range []struct {
		name            string
		admin           string
		wantPoolOptions bool
	}{
		{name: "configured URL", admin: admin},
		{
			name:            "with pool and session options",
			admin:           admin + separator + "pool_max_conns=4&application_name=" + applicationName,
			wantPoolOptions: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvTestDatabaseURL, tc.admin)
			var name string
			if !t.Run("create", func(t *testing.T) {
				d := New(t)
				name = d.Pool.Config().ConnConfig.Database
				if !databaseExists(t, tc.admin, name) {
					t.Fatalf("database %s missing while the test runs", name)
				}
				var migrated bool
				if err := d.Pool.QueryRow(t.Context(), "SELECT to_regclass('billing_accounts') IS NOT NULL").Scan(&migrated); err != nil {
					t.Fatalf("check billing_accounts migration: %v", err)
				}
				if !migrated {
					t.Error("billing_accounts table is missing")
				}
				if tc.wantPoolOptions {
					if got := d.Pool.Stat().MaxConns(); got != 4 {
						t.Errorf("pool maximum connections = %d, want 4", got)
					}
					var got string
					if err := d.Pool.QueryRow(t.Context(), "SHOW application_name").Scan(&got); err != nil {
						t.Fatalf("read application_name: %v", err)
					}
					if got != applicationName {
						t.Errorf("application_name = %q, want %q", got, applicationName)
					}
				}
			}) {
				return
			}
			if databaseExists(t, tc.admin, name) {
				t.Errorf("database %s still exists after cleanup", name)
			}
		})
	}
}

func TestCleanupDatabaseSucceeds(t *testing.T) {
	admin := URL(t)
	var name string
	if !t.Run("create", func(t *testing.T) {
		child := NewEmptyURL(t)
		cfg, err := pgx.ParseConfig(child)
		if err != nil {
			t.Fatal("parse child database configuration failed")
		}
		name = cfg.Database
		if !databaseExists(t, admin, name) {
			t.Fatalf("database %s missing while the test runs", name)
		}
	}) {
		return
	}
	if databaseExists(t, admin, name) {
		t.Fatalf("database %s still exists after cleanup", name)
	}

	rec := &recordingTB{TB: t}
	cleanupDatabase(rec, admin, name)
	if len(rec.errors) != 0 {
		t.Errorf("cleanup of a missing database reported errors: %q", rec.errors)
	}
	if databaseExists(t, admin, name) {
		t.Errorf("database %s exists after the second cleanup", name)
	}
}

func databaseExists(t *testing.T, admin, name string) bool {
	t.Helper()
	ctx := context.Background()
	conn, err := connectAdmin(ctx, admin, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		t.Fatalf("query pg_database: %v", err)
	}
	return exists
}
