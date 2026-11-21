// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/delinoio/mono/servers/accounts-api/internal/db"
	"github.com/delinoio/mono/servers/accounts-api/internal/db/dbtest"
)

func discard() *slog.Logger { return slog.New(slog.NewJSONHandler(io.Discard, nil)) }

func TestOpenRejectsMalformedURLWithoutEchoingIt(t *testing.T) {
	_, err := db.Open(context.Background(), "postgres://user:hunter2@127.0.0.1:5432/x?sslmode=bogus")
	if err == nil {
		t.Fatal("Open accepted a malformed URL")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %q contains the password", err)
	}
}

func TestOpenDoesNotConnect(t *testing.T) {
	// Port 1 on loopback refuses connections; Open must still succeed.
	d, err := db.Open(context.Background(), "postgres://user@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	d.Close()
}

func TestMigrateAppliesEveryMigrationOnceUnderConcurrency(t *testing.T) {
	url := dbtest.NewEmptyURL(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 3)
	var logs [3]bytes.Buffer
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := db.Open(ctx, url)
			if err != nil {
				errs <- err
				return
			}
			defer d.Close()
			errs <- d.Migrate(ctx, slog.New(slog.NewJSONHandler(&logs[i], nil)))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("Migrate() error = %v", err)
		}
	}
	applied := 0
	for i := range logs {
		applied += strings.Count(logs[i].String(), `"db_migration_applied"`)
	}
	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var versions int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id > 0").Scan(&versions); err != nil {
		t.Fatalf("read goose_db_version: %v", err)
	}
	if applied != versions {
		t.Fatalf("%d migrations logged as applied across instances, %d recorded", applied, versions)
	}
	if versions == 0 {
		t.Fatal("no migration was applied")
	}
	// A second run finds nothing to apply.
	var again bytes.Buffer
	if err := d.Migrate(ctx, slog.New(slog.NewJSONHandler(&again, nil))); err != nil {
		t.Fatalf("second Migrate() error = %v", err)
	}
	if strings.Contains(again.String(), "db_migration_applied") {
		t.Fatalf("second run applied migrations: %s", again.String())
	}
}

func TestSchemaKeepsOneOwnerPerAccount(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	const account = "01900000-0000-7000-8000-000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time) VALUES ($1, 'a', now(), now())`, account); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_account_members VALUES ($1, 'u1', 'owner', now())`, account); err != nil {
		t.Fatal(err)
	}
	_, err := d.Pool.Exec(ctx, `INSERT INTO billing_account_members VALUES ($1, 'u2', 'owner', now())`, account)
	if !db.IsUniqueViolation(err) {
		t.Fatalf("second owner error = %v, want a unique violation", err)
	}
	if err := d.Migrate(ctx, discard()); err != nil {
		t.Fatal(err)
	}
}
