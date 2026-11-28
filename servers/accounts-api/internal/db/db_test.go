// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

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
	// Skip pgx service lookup so the fixture does not require a local service file.
	t.Setenv("PGSERVICE", "")

	// Port 1 on loopback refuses connections; Open must still succeed.
	d, err := db.Open(context.Background(), "postgres://user@127.0.0.1:1/none?connect_timeout=1")
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	d.Close()
}

// The accounts-api storage contract specifies this advisory lock key.
const accountsMigrationLockID int64 = 0x6163636f756e7473

func TestMigrateAppliesEveryMigrationOnceUnderConcurrency(t *testing.T) {
	url := dbtest.NewEmptyURL(t)
	want := embeddedMigrationVersions(t)
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	holder, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect migration lock holder: %v", err)
	}
	defer holder.Close(ctx)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", accountsMigrationLockID); err != nil {
		t.Fatalf("hold accounts migration lock: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse migration worker pool config: %v", err)
	}
	// Goose polls with pg_try_advisory_lock, so there is no waiting row in
	// pg_locks. A completed probe on a separate session with the same key
	// must have returned false while the holder still owns the lock.
	tracer := &migrationLockTracer{attempted: make(chan struct{})}
	cfg.ConnConfig.Tracer = tracer
	var logs [3]bytes.Buffer
	var errs [len(logs)]error
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	for i := range logs {
		wg.Go(func() {
			pool, err := pgxpool.NewWithConfig(ctx, cfg.Copy())
			if err != nil {
				errs[i] = err
				return
			}
			d := &db.DB{Pool: pool}
			defer d.Close()
			errs[i] = d.Migrate(ctx, slog.New(slog.NewJSONHandler(&logs[i], nil)))
		})
	}

	select {
	case <-tracer.attempted:
		t.Log("observed a competing attempt while the accounts migration lock was held")
	case <-time.After(10 * time.Second):
		t.Fatal("no competing attempt on the accounts migration lock within 10 seconds while the holder owned it")
	}
	var released bool
	if err := holder.QueryRow(ctx, "SELECT pg_advisory_unlock($1)", accountsMigrationLockID).Scan(&released); err != nil || !released {
		t.Fatalf("release accounts migration lock: released = %t, error = %v", released, err)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Migrate() call %d: %v", i+1, err)
		}
	}

	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.Pool.Query(ctx, "SELECT version_id FROM goose_db_version WHERE is_applied AND version_id <> 0")
	if err != nil {
		t.Fatalf("query applied migration versions: %v", err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatalf("read applied migration versions: %v", err)
	}
	recorded := make(map[int64]int)
	for _, version := range versions {
		recorded[version]++
	}
	assertMigrationVersions(t, "database", want, recorded)
	assertMigrationVersions(t, "logs", want, loggedMigrationVersions(t, io.MultiReader(&logs[0], &logs[1], &logs[2])))

	var again bytes.Buffer
	if err := d.Migrate(ctx, slog.New(slog.NewJSONHandler(&again, nil))); err != nil {
		t.Fatalf("subsequent Migrate() error = %v", err)
	}
	assertMigrationVersions(t, "subsequent run logs", nil, loggedMigrationVersions(t, &again))
}

type migrationLockTracer struct {
	attempted chan struct{}
	once      sync.Once
}

type migrationLockProbeKey struct{}

func (*migrationLockTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	// pgx's database/sql adapter prepends a result-format option.
	if data.SQL == "SELECT pg_try_advisory_lock($1)" && len(data.Args) > 0 && data.Args[len(data.Args)-1] == accountsMigrationLockID {
		return context.WithValue(ctx, migrationLockProbeKey{}, true)
	}
	return ctx
}

func (tracer *migrationLockTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryEndData) {
	if ctx.Value(migrationLockProbeKey{}) == true && data.Err == nil && data.CommandTag.RowsAffected() == 1 {
		tracer.once.Do(func() { close(tracer.attempted) })
	}
}

func embeddedMigrationVersions(t *testing.T) map[int64]bool {
	t.Helper()
	files, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil {
		t.Fatalf("list embedded migrations: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("no embedded migrations")
	}
	versions := make(map[int64]bool, len(files))
	for _, name := range files {
		prefix, _, found := strings.Cut(name, "_")
		version, err := strconv.ParseInt(prefix, 10, 64)
		if !found || err != nil || version <= 0 {
			t.Fatalf("invalid embedded migration version in %q", name)
		}
		if versions[version] {
			t.Fatalf("duplicate embedded migration version %d in %q", version, name)
		}
		versions[version] = true
	}
	return versions
}

func loggedMigrationVersions(t *testing.T, logs io.Reader) map[int64]int {
	t.Helper()
	counts := make(map[int64]int)
	decoder := json.NewDecoder(logs)
	for {
		var event struct {
			Message string `json:"msg"`
			Version int64  `json:"version"`
		}
		err := decoder.Decode(&event)
		if errors.Is(err, io.EOF) {
			return counts
		}
		if err != nil {
			t.Fatalf("decode structured migration log: %v", err)
		}
		if event.Message == "db_migration_applied" {
			counts[event.Version]++
		}
	}
}

func assertMigrationVersions(t *testing.T, source string, want map[int64]bool, counts map[int64]int) {
	t.Helper()
	for _, version := range slices.Sorted(maps.Keys(want)) {
		if counts[version] == 0 {
			t.Errorf("%s: missing migration version %d", source, version)
		}
	}
	for _, version := range slices.Sorted(maps.Keys(counts)) {
		if !want[version] {
			t.Errorf("%s: unexpected migration version %d", source, version)
		}
		if counts[version] > 1 {
			t.Errorf("%s: duplicate migration version %d (%d entries)", source, version, counts[version])
		}
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
	const blocked = "01900000-0000-7000-8000-000000000302"
	if err := insertLink(ctx, d, blocked, account, "blocked", "no_active_subscription", int64(2), now, now.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	const withoutStatus = "01900000-0000-7000-8000-000000000303"
	if err := insertLink(ctx, d, withoutStatus, account, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	type linkSnapshot struct {
		Service, OrganizationID, BillingAccountID string
		Status, Reason                            pgtype.Text
		Sequence                                  pgtype.Int8
		BlockedSince, SuspendTime                 pgtype.Timestamptz
	}
	readLink := func(id, phase string) linkSnapshot {
		t.Helper()
		var link linkSnapshot
		err := d.Pool.QueryRow(ctx, `SELECT service, organization_id, billing_account_id::text,
			billing_status, status_reason, status_sequence, blocked_since, suspend_time
			FROM organization_links WHERE organization_link_id = $1`, id).Scan(
			&link.Service, &link.OrganizationID, &link.BillingAccountID,
			&link.Status, &link.Reason, &link.Sequence, &link.BlockedSince, &link.SuspendTime)
		if err != nil {
			t.Fatalf("read organization link %s %s migration: %v", id, phase, err)
		}
		link.BlockedSince.Time = link.BlockedSince.Time.UTC()
		link.SuspendTime.Time = link.SuspendTime.Time.UTC()
		return link
	}
	ids := []string{blocked, withoutStatus}
	before := make(map[string]linkSnapshot, len(ids))
	for _, id := range ids {
		before[id] = readLink(id, "before")
	}
	if err := d.Migrate(ctx, discard()); err != nil {
		t.Fatalf("migrate with complete rows: %v", err)
	}
	for _, id := range ids {
		if got, want := readLink(id, "after"), before[id]; got != want {
			t.Errorf("organization link %s changed during migration:\n got %+v\nwant %+v", id, got, want)
		}
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
