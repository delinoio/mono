// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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

// insertLink inserts a link row of the account with the given status columns;
// a nil value is stored as NULL.
func insertLink(ctx context.Context, d *db.DB, id, account string, status, reason, sequence, blockedSince, suspendTime any) error {
	_, err := d.Pool.Exec(ctx, `INSERT INTO organization_links
		(organization_link_id, service, organization_id, billing_account_id, link_time,
		 billing_status, status_reason, status_sequence, blocked_since, suspend_time)
		VALUES ($1, 'ownsand', $2, $3, now(), $4, $5, $6, $7, $8)`,
		id, id, account, status, reason, sequence, blockedSince, suspendTime)
	return err
}

// isStatusCompleteViolation reports whether err is a violation of the
// organization_links_status_complete check constraint.
func isStatusCompleteViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23514" &&
		pgErr.ConstraintName == "organization_links_status_complete"
}

func TestOrganizationLinkStatusMustBeComplete(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	const account = "01900000-0000-7000-8000-000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time) VALUES ($1, 'a', now(), now())`, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	rejected := []struct {
		name                                                string
		status, reason, sequence, blockedSince, suspendTime any
	}{
		{"blocked with a NULL reason", "blocked", nil, int64(1), now, now},
		{"suspended with a NULL reason", "suspended", nil, int64(1), now, now},
		{"allowed with a NULL reason", "allowed", nil, int64(1), nil, nil},
		{"NULL status with reason none and a sequence", nil, "none", int64(1), nil, nil},
		{"NULL status with a reason and both timestamps", nil, "no_active_subscription", int64(1), now, now},
		{"NULL status with only a sequence", nil, nil, int64(1), nil, nil},
		{"NULL status with only blocked_since", nil, nil, nil, now, nil},
		{"NULL status with only suspend_time", nil, nil, nil, nil, now},
		{"blocked without a sequence", "blocked", "non_positive_balance", nil, now, now},
		{"blocked without timestamps", "blocked", "non_positive_balance", int64(1), nil, nil},
		{"allowed with a timestamp", "allowed", "none", int64(1), now, nil},
		{"blocked with reason none", "blocked", "none", int64(1), now, now},
	}
	for i, c := range rejected {
		id := fmt.Sprintf("01900000-0000-7000-8000-0000000001%02d", i)
		if err := insertLink(ctx, d, id, account, c.status, c.reason, c.sequence, c.blockedSince, c.suspendTime); !isStatusCompleteViolation(err) {
			t.Errorf("insert %s: error = %v, want a violation of organization_links_status_complete", c.name, err)
		}
	}
	accepted := []struct {
		name                                                string
		status, reason, sequence, blockedSince, suspendTime any
	}{
		{"no status", nil, nil, nil, nil, nil},
		{"allowed", "allowed", "none", int64(2), nil, nil},
		{"blocked", "blocked", "no_active_subscription", int64(3), now, now},
		{"suspended", "suspended", "non_positive_balance", int64(4), now, now},
	}
	for i, c := range accepted {
		id := fmt.Sprintf("01900000-0000-7000-8000-0000000002%02d", i)
		if err := insertLink(ctx, d, id, account, c.status, c.reason, c.sequence, c.blockedSince, c.suspendTime); err != nil {
			t.Errorf("insert %s: %v", c.name, err)
		}
	}
	// An UPDATE that leaves a complete row incomplete is rejected as well.
	_, err := d.Pool.Exec(ctx, `UPDATE organization_links SET status_reason = NULL WHERE organization_link_id = '01900000-0000-7000-8000-000000000201'`)
	if !isStatusCompleteViolation(err) {
		t.Errorf("update to a NULL reason: error = %v, want a violation of organization_links_status_complete", err)
	}
	_, err = d.Pool.Exec(ctx, `UPDATE organization_links SET billing_status = NULL WHERE organization_link_id = '01900000-0000-7000-8000-000000000201'`)
	if !isStatusCompleteViolation(err) {
		t.Errorf("update to a NULL status: error = %v, want a violation of organization_links_status_complete", err)
	}
}

func TestOrganizationLinkStatusMigrationKeepsCompleteRows(t *testing.T) {
	ctx := context.Background()
	// Migration 00004 is the last one before the stricter constraint.
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 4))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	const account = "01900000-0000-7000-8000-000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time) VALUES ($1, 'a', now(), now())`, account); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	// The constraint of migration 00003 accepts a blocked link without a reason.
	const incomplete = "01900000-0000-7000-8000-000000000301"
	if err := insertLink(ctx, d, incomplete, account, "blocked", nil, int64(1), now, now); err != nil {
		t.Fatalf("insert an incomplete row before migration 00005: %v", err)
	}
	if _, err := d.Pool.Exec(ctx, `DELETE FROM organization_links WHERE organization_link_id = $1`, incomplete); err != nil {
		t.Fatal(err)
	}
	if err := insertLink(ctx, d, "01900000-0000-7000-8000-000000000302", account, "blocked", "no_active_subscription", int64(2), now, now); err != nil {
		t.Fatal(err)
	}
	if err := insertLink(ctx, d, "01900000-0000-7000-8000-000000000303", account, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, discard()); err != nil {
		t.Fatalf("migrate with complete rows: %v", err)
	}
	if err := insertLink(ctx, d, incomplete, account, "blocked", nil, int64(1), now, now); !isStatusCompleteViolation(err) {
		t.Fatalf("insert an incomplete row after migration 00005: error = %v, want a violation of organization_links_status_complete", err)
	}
}

func TestTopUpCapTimeMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	// Migration 00005 is the last one before the cap time column.
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 5))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	const account = "01900000-0000-7000-8000-000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time) VALUES ($1, 'a', now(), now())`, account); err != nil {
		t.Fatal(err)
	}
	const pending = "01900000-0000-7000-8000-000000000401"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO top_ups (top_up_id, billing_account_id, kind, amount_usd_micros, status, create_time, update_time)
		VALUES ($1, $2, 'auto', 25000000, 'pending', '2026-12-31T23:59:00Z', '2026-12-31T23:59:00Z')`, pending, account); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, discard()); err != nil {
		t.Fatalf("migrate with an existing pending top-up: %v", err)
	}
	// The existing row has no cap time, so the month-to-date total falls back
	// to its create_time.
	var capTime *time.Time
	var effective time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT cap_time, COALESCE(cap_time, create_time) FROM top_ups WHERE top_up_id = $1`, pending).Scan(&capTime, &effective); err != nil {
		t.Fatal(err)
	}
	if capTime != nil {
		t.Fatalf("cap_time = %v, want NULL for an existing row", *capTime)
	}
	if want := time.Date(2026, 12, 31, 23, 59, 0, 0, time.UTC); !effective.Equal(want) {
		t.Fatalf("effective cap time = %v, want create_time %v", effective, want)
	}
}

func TestTopUpProviderAcceptTimeMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	// Migration 00006 is the last one before the provider acceptance column.
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 6))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	const account = "01900000-0000-7000-8000-000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time) VALUES ($1, 'a', now(), now())`, account); err != nil {
		t.Fatal(err)
	}
	const legacy = "01900000-0000-7000-8000-000000000501"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO top_ups (top_up_id, billing_account_id, kind, idempotency_key, amount_usd_micros, status, failure_reason, create_time, update_time)
		VALUES ($1, $2, 'manual', '01900000-0000-7000-8000-000000000502', 25000000, 'failed', 'timed_out', now(), now())`, legacy, account); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, discard()); err != nil {
		t.Fatalf("migrate with an existing timed-out top-up: %v", err)
	}
	// The existing row has no evidence that Paddle accepted the charge.
	var acceptTime *time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT provider_accept_time FROM top_ups WHERE top_up_id = $1`, legacy).Scan(&acceptTime); err != nil {
		t.Fatal(err)
	}
	if acceptTime != nil {
		t.Fatalf("provider_accept_time = %v, want NULL for an existing row", *acceptTime)
	}
}

func TestLedgerTimeIndexMigrationKeepsExistingRows(t *testing.T) {
	ctx := context.Background()
	// Migration 00007 is the last one before the ledger time index.
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	const account = "01900000-0000-7000-8000-000000000001"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time) VALUES ($1, 'a', now(), now())`, account); err != nil {
		t.Fatal(err)
	}
	const entry = "01900000-0000-7000-8000-000000000601"
	if _, err := d.Pool.Exec(ctx, `INSERT INTO ledger_entries (ledger_entry_id, billing_account_id, kind, amount_usd_micros, balance_after_usd_micros, create_time)
		VALUES ($1, $2, 'manual_top_up', 5000000, 5000000, '2026-01-02 03:04:05.678901+00')`, entry, account); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, discard()); err != nil {
		t.Fatalf("migrate with an existing ledger entry: %v", err)
	}
	// The migration adds the index and leaves the row as it was.
	var indexes int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_indexes WHERE tablename = 'ledger_entries'
		AND indexname IN ('ledger_entries_by_account', 'ledger_entries_by_account_time')`).Scan(&indexes); err != nil {
		t.Fatal(err)
	}
	if indexes != 2 {
		t.Fatalf("ledger indexes = %d, want the ID index and the time index", indexes)
	}
	var amount int64
	var createTime time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT amount_usd_micros, create_time FROM ledger_entries WHERE ledger_entry_id = $1`, entry).Scan(&amount, &createTime); err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 678901000, time.UTC); amount != 5000000 || !createTime.Equal(want) {
		t.Fatalf("ledger entry = %d at %v, want 5000000 at %v", amount, createTime, want)
	}
}
