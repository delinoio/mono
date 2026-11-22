// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"io/fs"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

const insertInventoryDevice = `INSERT INTO devices (device_id, organization_id, public_key, name, host_os, arch, agent_version,
	update_state, update_target_version, pending_idle_since, capabilities, limit_vcpu_count, limit_memory_mib, limit_disk_gib,
	vcpu_overcommit_percent, state, state_epoch, create_time, last_seen_time, remove_time, tombstone_expire_time)
	VALUES ($1, $2, '\x0102', 'build-box', 2, 2, '1.2.3', 2, '1.3.0', now(), '\x0a00', 8, 4096, 100, 150, $3::smallint, 'epoch-1', now(), now(),
	CASE WHEN $3::smallint = 3 THEN now() END, CASE WHEN $3::smallint = 3 THEN now() + interval '1 year' END)`

// scrubbedRow reads the columns that the scrub trigger resets plus the ones
// it keeps, as text, so a test can compare whole rows.
const scrubbedRow = `SELECT name, host_os, arch, agent_version, update_state, update_target_version, pending_idle_since IS NULL,
	capabilities IS NULL, limit_vcpu_count, limit_memory_mib, limit_disk_gib, vcpu_overcommit_percent, state_epoch,
	last_seen_time IS NULL FROM devices WHERE device_id = $1`

type inventory struct {
	name, agentVersion, updateTarget, epoch string
	hostOS, arch, updateState               int16
	idleNull, capsNull, seenNull            bool
	vcpu, overcommit                        int32
	memory, disk                            int64
}

func readInventory(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, id string) inventory {
	t.Helper()
	var v inventory
	if err := q.QueryRow(ctx, scrubbedRow, id).Scan(&v.name, &v.hostOS, &v.arch, &v.agentVersion, &v.updateState, &v.updateTarget, &v.idleNull,
		&v.capsNull, &v.vcpu, &v.memory, &v.disk, &v.overcommit, &v.epoch, &v.seenNull); err != nil {
		t.Fatal(err)
	}
	return v
}

// scrubbed is the inventory of a tombstone: empty strings, zeros, NULLs and
// the initial default of vcpu_overcommit_percent.
var scrubbed = inventory{idleNull: true, capsNull: true, seenNull: true, overcommit: 100}

// TestScrubRemovedDeviceTrigger checks that no write stores inventory on a
// removed row, while an active row keeps it and the tombstone keeps its
// identity and expiry columns.
func TestScrubRemovedDeviceTrigger(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	org := "018f0000-0000-7000-8000-000000000001"
	active, removed := "018f0000-0000-7000-8000-0000000000a1", "018f0000-0000-7000-8000-0000000000a2"
	if _, err := d.Pool.Exec(ctx, insertInventoryDevice, active, org, int16(1)); err != nil {
		t.Fatal(err)
	}
	// An insert with state REMOVED is scrubbed too.
	if _, err := d.Pool.Exec(ctx, insertInventoryDevice, removed, org, int16(3)); err != nil {
		t.Fatal(err)
	}
	if got := readInventory(t, ctx, d.Pool, removed); got != scrubbed {
		t.Fatalf("inserted tombstone inventory = %+v", got)
	}
	if got := readInventory(t, ctx, d.Pool, active); got.name != "build-box" || got.vcpu != 8 || got.overcommit != 150 || got.capsNull || got.epoch != "epoch-1" {
		t.Fatalf("active device inventory = %+v", got)
	}
	// Late writers: a capability report, built images merge, heartbeat
	// update fields and a last-seen update on the tombstone.
	for _, stmt := range []string{
		"UPDATE devices SET capabilities = '\\x0a00' WHERE device_id = $1",
		"UPDATE devices SET name = 'late', agent_version = '9', limit_vcpu_count = 4, vcpu_overcommit_percent = 200 WHERE device_id = $1",
		"UPDATE devices SET update_state = 3, update_target_version = '2.0.0', pending_idle_since = now(), last_seen_time = now(), state_epoch = 'late' WHERE device_id = $1",
	} {
		if _, err := d.Pool.Exec(ctx, stmt, removed); err != nil {
			t.Fatal(err)
		}
		if got := readInventory(t, ctx, d.Pool, removed); got != scrubbed {
			t.Fatalf("after %q inventory = %+v", stmt, got)
		}
	}
	// Removing an active device scrubs it and keeps identity and expiry.
	if _, err := d.Pool.Exec(ctx, "UPDATE devices SET state = 3, remove_time = now(), tombstone_expire_time = now() + interval '1 year' WHERE device_id = $1", active); err != nil {
		t.Fatal(err)
	}
	if got := readInventory(t, ctx, d.Pool, active); got != scrubbed {
		t.Fatalf("removed device inventory = %+v", got)
	}
	var kept int
	if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM devices WHERE state = 3 AND organization_id = $1 AND public_key = '\x0102'
		AND create_time IS NOT NULL AND remove_time IS NOT NULL AND tombstone_expire_time IS NOT NULL`, org).Scan(&kept); err != nil || kept != 2 {
		t.Fatalf("tombstones with identity columns = %d, %v", kept, err)
	}
}

// TestScrubRemovedDevicesMigration applies migrations 1 to 4, stores a
// legacy tombstone that holds inventory plus an active device, and then
// applies the later migrations through the runner.
func TestScrubRemovedDevicesMigration(t *testing.T) {
	url := dbtest.NewURLAtVersion(t, 4)
	ctx := context.Background()
	d, err := db.Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	org := "018f0000-0000-7000-8000-000000000001"
	active, legacy := "018f0000-0000-7000-8000-0000000000b1", "018f0000-0000-7000-8000-0000000000b2"
	if _, err := d.Pool.Exec(ctx, insertInventoryDevice, active, org, int16(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, insertInventoryDevice, legacy, org, int16(3)); err != nil {
		t.Fatal(err)
	}
	if got := readInventory(t, ctx, d.Pool, legacy); got.name != "build-box" || got.capsNull {
		t.Fatalf("legacy tombstone should hold inventory before the migration, got %+v", got)
	}
	var expire time.Time
	if err := d.Pool.QueryRow(ctx, "SELECT tombstone_expire_time FROM devices WHERE device_id = $1", legacy).Scan(&expire); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}
	if got := readInventory(t, ctx, d.Pool, legacy); got != scrubbed {
		t.Fatalf("legacy tombstone inventory after the migration = %+v", got)
	}
	var after time.Time
	var state int16
	var key []byte
	if err := d.Pool.QueryRow(ctx, "SELECT tombstone_expire_time, state, public_key FROM devices WHERE device_id = $1", legacy).Scan(&after, &state, &key); err != nil {
		t.Fatal(err)
	}
	if !after.Equal(expire) || state != 3 || len(key) != 2 {
		t.Fatalf("tombstone identity after the migration: expire %v (was %v), state %d, key %x", after, expire, state, key)
	}
	if got := readInventory(t, ctx, d.Pool, active); got.name != "build-box" || got.vcpu != 8 || got.capsNull {
		t.Fatalf("active device after the migration = %+v", got)
	}
}

// appliedVersions returns the applied versions in the goose version table,
// including the zero version, in insertion order.
func appliedVersions(t *testing.T, ctx context.Context, d *db.DB) []int64 {
	t.Helper()
	rows, err := d.Pool.Query(ctx, "SELECT version_id FROM goose_db_version WHERE is_applied ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	versions, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.Fatal(err)
	}
	return versions
}

// embeddedVersions returns 0 followed by the version of every embedded
// migration file, in order.
func embeddedVersions(t *testing.T) []int64 {
	t.Helper()
	files, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil || len(files) == 0 {
		t.Fatalf("embedded migrations = %v, %v", files, err)
	}
	versions := []int64{0}
	for _, f := range files {
		prefix, _, _ := strings.Cut(f, "_")
		v, err := strconv.ParseInt(prefix, 10, 64)
		if err != nil {
			t.Fatalf("migration %s: %v", f, err)
		}
		versions = append(versions, v)
	}
	return versions
}

// TestMigrateIsIdempotent applies the migrations to a migrated database
// again and checks that goose records every embedded migration once.
func TestMigrateIsIdempotent(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}
	if got, want := appliedVersions(t, ctx, d), embeddedVersions(t); !slices.Equal(got, want) {
		t.Fatalf("goose_db_version versions = %v, want %v", got, want)
	}
}

// setDatabaseTimeZone sets the default time zone of new sessions of the test
// database.
func setDatabaseTimeZone(t *testing.T, ctx context.Context, d *db.DB, zone string) {
	t.Helper()
	var name string
	if err := d.Pool.QueryRow(ctx, "SELECT current_database()").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Pool.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{name}.Sanitize()+" SET timezone = "+pgx.Identifier{zone}.Sanitize()); err != nil {
		t.Fatal(err)
	}
}

// deliveryIDMigration is the version of 0006_control_events_delivery_id.sql.
const deliveryIDMigration = 6

// TestDeliveryIDMigrationKeepsOldWritersWorking applies the migrations up to
// the one before the delivery ID migration, writes events the way the
// previous release does, applies the delivery ID migration and checks the
// backfill, the sequence and an event that an old instance inserts while a
// new instance polls (a rolling deployment).
func TestDeliveryIDMigrationKeepsOldWritersWorking(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, deliveryIDMigration-1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	// oldInsert is the INSERT statement of the previous release.
	oldInsert := func() {
		t.Helper()
		if _, err := d.Pool.Exec(ctx, "INSERT INTO control_events (kind, device_id) VALUES (4, gen_random_uuid())"); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		oldInsert()
	}
	// A janitor delete leaves a gap below the newest event_id.
	if _, err := d.Pool.Exec(ctx, "DELETE FROM control_events WHERE event_id = 1"); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}

	var mismatched int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM control_events WHERE delivery_id IS DISTINCT FROM event_id").Scan(&mismatched); err != nil || mismatched != 0 {
		t.Fatalf("rows with delivery_id != event_id = %d, %v", mismatched, err)
	}
	latest, err := events.LatestID(ctx, d.Pool)
	if err != nil || latest != 3 {
		t.Fatalf("LatestID = %d, %v, want 3", latest, err)
	}

	// The lease holder starts at LatestID; an old instance then inserts.
	oldInsert()
	got, err := events.After(ctx, d.Pool, latest, 500)
	if err != nil || len(got) != 1 || got[0].ID != latest+1 || got[0].Kind != events.KindDeviceConfigChanged {
		t.Fatalf("After(%d) = %+v, %v, want the event of the old instance with delivery ID %d", latest, got, err, latest+1)
	}
	// The old release keeps its event_id cursor.
	var eventID int64
	if err := d.Pool.QueryRow(ctx, "SELECT event_id FROM control_events WHERE delivery_id = $1", got[0].ID).Scan(&eventID); err != nil || eventID <= latest {
		t.Fatalf("event_id = %d, %v, want an ID above %d", eventID, err, latest)
	}
	// No replay at the next poll, and the next event follows.
	oldInsert()
	got, err = events.After(ctx, d.Pool, latest+1, 500)
	if err != nil || len(got) != 1 || got[0].ID != latest+2 {
		t.Fatalf("After(%d) = %+v, %v, want only delivery ID %d", latest+1, got, err, latest+2)
	}
}

// TestDeliveryIDMigrationOnEmptyTable checks the sequence start on a database
// without events.
func TestDeliveryIDMigrationOnEmptyTable(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if _, err := d.Pool.Exec(ctx, "INSERT INTO control_events (kind) VALUES (4)"); err != nil {
		t.Fatal(err)
	}
	if id, err := events.LatestID(ctx, d.Pool); err != nil || id != 1 {
		t.Fatalf("LatestID = %d, %v, want 1", id, err)
	}
}

// billingStatusMigration is the version of 0008_billing_status.sql, and
// legacyWriterMigration the version of
// 0009_billing_status_legacy_writers.sql.
const (
	billingStatusMigration = 8
	legacyWriterMigration  = 9
)

// legacyOrg is the INSERT statement of the release before billing: an
// organization without a status row.
const legacyOrg = "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, 'org', $2)"

type billingRow struct {
	account               *string
	phase, reason         int16
	blockedSince, suspend *time.Time
	syncTime              *time.Time
	sequence              int64
	updateTime            time.Time
	count                 int
}

// readBilling reads the status row of an organization and the number of
// rows it has.
func readBilling(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, org string) billingRow {
	t.Helper()
	var r billingRow
	if err := q.QueryRow(ctx, "SELECT count(*) FROM organization_billing WHERE organization_id = $1", org).Scan(&r.count); err != nil {
		t.Fatal(err)
	}
	if r.count == 0 {
		return r
	}
	if err := q.QueryRow(ctx, `SELECT billing_account_id::text, phase, reason, blocked_since, suspend_time, status_sync_time, status_sequence, update_time
		FROM organization_billing WHERE organization_id = $1`, org).Scan(&r.account, &r.phase, &r.reason, &r.blockedSince, &r.suspend, &r.syncTime, &r.sequence, &r.updateTime); err != nil {
		t.Fatal(err)
	}
	return r
}

// wantBlocked checks a BLOCKED/ACCOUNT_REQUIRED row without a link whose
// block starts at start and whose suspension follows 72 hours later.
func wantBlocked(t *testing.T, r billingRow, start time.Time) {
	t.Helper()
	if r.count != 1 || r.account != nil || r.phase != 2 || r.reason != 1 || r.syncTime != nil || r.sequence != 0 {
		t.Fatalf("status row = %+v, want one unlinked BLOCKED/ACCOUNT_REQUIRED row", r)
	}
	if r.blockedSince == nil || !r.blockedSince.Equal(start) {
		t.Fatalf("blocked_since = %v, want %v", r.blockedSince, start)
	}
	if r.suspend == nil || !r.suspend.Equal(start.Add(72*time.Hour)) {
		t.Fatalf("suspend_time = %v, want %v", r.suspend, start.Add(72*time.Hour))
	}
}

// appliedAt returns the time at which goose recorded the migration. goose
// stores it as UTC without a time zone (see db.MigrateUpTo).
func appliedAt(t *testing.T, ctx context.Context, d *db.DB, version int64) time.Time {
	t.Helper()
	var at time.Time
	if err := d.Pool.QueryRow(ctx, `SELECT tstamp AT TIME ZONE 'UTC' FROM goose_db_version
		WHERE version_id = $1 AND is_applied ORDER BY id DESC LIMIT 1`, version).Scan(&at); err != nil {
		t.Fatal(err)
	}
	return at
}

// TestLegacyWriterMigrationBackfill upgrades a database at the schema of
// migration 0008 that holds an organization from before billing, a linked
// organization and an organization that a legacy writer created after
// migration 0008, and checks the backfill and that existing rows stay.
func TestLegacyWriterMigrationBackfill(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, billingStatusMigration-1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	old, linked, legacyOld, legacyNew := "018f0000-0000-7000-8000-0000000000c1", "018f0000-0000-7000-8000-0000000000c2",
		"018f0000-0000-7000-8000-0000000000c3", "018f0000-0000-7000-8000-0000000000c4"
	for _, org := range []string{old, linked} {
		if _, err := d.Pool.Exec(ctx, legacyOrg, org, time.Now().Add(-240*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	// goose records the application time from now() of the session; the
	// migration sessions must store it as UTC whatever the database time
	// zone is.
	setDatabaseTimeZone(t, ctx, d, "Pacific/Kiritimati")
	if err := d.MigrateUpTo(ctx, testenv.Discard(), billingStatusMigration); err != nil {
		t.Fatal(err)
	}
	applied := appliedAt(t, ctx, d, billingStatusMigration)
	// Migration 0008 started the block of the existing organizations at its
	// own now(), the time that goose recorded for it.
	if got := readBilling(t, ctx, d.Pool, old); got.blockedSince == nil || !got.blockedSince.Equal(applied) {
		t.Fatalf("blocked_since after migration %d = %v, want its recorded application time %v", billingStatusMigration, got.blockedSince, applied)
	}
	// Migration 0008 backfilled the two organizations; one of them links.
	linkedAt := time.Date(2031, 1, 2, 3, 4, 5, 0, time.UTC)
	if _, err := d.Pool.Exec(ctx, `UPDATE organization_billing SET billing_account_id = '01900000-0000-7000-8000-00000000b111', phase = 1, reason = 0,
		blocked_since = NULL, suspend_time = NULL, status_sync_time = $2, status_sequence = 7, update_time = $2 WHERE organization_id = $1`, linked, linkedAt); err != nil {
		t.Fatal(err)
	}
	beforeOld, beforeLinked := readBilling(t, ctx, d.Pool, old), readBilling(t, ctx, d.Pool, linked)
	// The previous release is still running: it creates organizations
	// without a status row. The first has a create time before migration
	// 0008 (the block then starts at the migration), the second after it
	// (the block starts at the creation).
	if _, err := d.Pool.Exec(ctx, legacyOrg, legacyOld, applied.Add(-48*time.Hour)); err != nil {
		t.Fatal(err)
	}
	newCreate := applied.Add(5 * time.Minute)
	if _, err := d.Pool.Exec(ctx, legacyOrg, legacyNew, newCreate); err != nil {
		t.Fatal(err)
	}
	if got := readBilling(t, ctx, d.Pool, legacyNew); got.count != 0 {
		t.Fatalf("the legacy writer's organization has a status row before migration %d: %+v", legacyWriterMigration, got)
	}
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}
	wantBlocked(t, readBilling(t, ctx, d.Pool, legacyOld), applied)
	wantBlocked(t, readBilling(t, ctx, d.Pool, legacyNew), newCreate)
	if got := readBilling(t, ctx, d.Pool, old); !reflect.DeepEqual(got, beforeOld) {
		t.Fatalf("existing unlinked row changed: %+v, was %+v", got, beforeOld)
	}
	if got := readBilling(t, ctx, d.Pool, linked); !reflect.DeepEqual(got, beforeLinked) || got.account == nil || got.sequence != 7 {
		t.Fatalf("linked row changed: %+v, was %+v", got, beforeLinked)
	}
	var orgs, rows int
	if err := d.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM organizations), (SELECT count(*) FROM organization_billing)").Scan(&orgs, &rows); err != nil || orgs != 4 || rows != 4 {
		t.Fatalf("organizations = %d, status rows = %d, %v, want 4 each", orgs, rows, err)
	}
	// The migration is recorded once and the runner accepts the database.
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}
}

// TestLegacyWriterTrigger checks the deferred trigger on a fully migrated
// database: a legacy writer's organization has exactly one unlinked status
// row when its transaction commits, a billing-aware writer's own row wins,
// and an organization that does not survive its transaction gets no row.
func TestLegacyWriterTrigger(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	created := time.Date(2030, 5, 6, 7, 8, 9, 0, time.UTC)
	const (
		legacy      = "018f0000-0000-7000-8000-0000000000d1"
		aware       = "018f0000-0000-7000-8000-0000000000d2"
		gone        = "018f0000-0000-7000-8000-0000000000d3"
		legacyTx    = "018f0000-0000-7000-8000-0000000000d4"
		awareLinked = "018f0000-0000-7000-8000-0000000000d5"
	)

	// A legacy writer in autocommit mode.
	if _, err := d.Pool.Exec(ctx, legacyOrg, legacy, created); err != nil {
		t.Fatal(err)
	}
	wantBlocked(t, readBilling(t, ctx, d.Pool, legacy), created)

	// A legacy writer in a transaction: no row inside it, one row after the
	// commit, and the membership insert that follows does not matter.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, legacyOrg, legacyTx, created); err != nil {
		t.Fatal(err)
	}
	if got := readBilling(t, ctx, tx, legacyTx); got.count != 0 {
		t.Fatalf("status row inside the transaction: %+v", got)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wantBlocked(t, readBilling(t, ctx, d.Pool, legacyTx), created)

	// A billing-aware writer inserts its row in the transaction; the trigger
	// keeps it (ON CONFLICT DO NOTHING).
	own := created.Add(time.Hour)
	err = d.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, legacyOrg, aware, created); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO organization_billing (organization_id, billing_account_id, phase, reason, blocked_since, suspend_time, update_time)
			VALUES ($1, NULL, 2, 1, $2, $3, $2)`, aware, own, own.Add(72*time.Hour))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	wantBlocked(t, readBilling(t, ctx, d.Pool, aware), own)

	// A billing-aware writer that also links in its transaction keeps the link.
	err = d.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, legacyOrg, awareLinked, created); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO organization_billing (organization_id, billing_account_id, phase, reason, update_time)
			VALUES ($1, '01900000-0000-7000-8000-00000000b111', 1, 0, $2)`, awareLinked, own)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readBilling(t, ctx, d.Pool, awareLinked); got.count != 1 || got.account == nil || got.phase != 1 {
		t.Fatalf("billing-aware linked row = %+v", got)
	}

	// An organization that is deleted in its own transaction gets no row,
	// and the commit does not fail on the foreign key.
	err = d.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, legacyOrg, gone, created); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", gone)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := readBilling(t, ctx, d.Pool, gone); got.count != 0 {
		t.Fatalf("status row of a deleted organization: %+v", got)
	}

	// A rolled back insert leaves nothing, and the organization delete
	// cascades to the row.
	if err := d.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, legacyOrg, gone, created); err != nil {
			return err
		}
		return context.Canceled
	}); err == nil {
		t.Fatal("the transaction was expected to roll back")
	}
	if _, err := d.Pool.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", legacy); err != nil {
		t.Fatal(err)
	}
	if got := readBilling(t, ctx, d.Pool, legacy); got.count != 0 {
		t.Fatalf("status row after the organization was deleted: %+v", got)
	}
	var rows int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM organization_billing WHERE organization_id = $1", gone).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("rows of a rolled back organization = %d, %v", rows, err)
	}
}

// waitForLockWaiters polls until n lock requests of the test database wait,
// so a test can order the sessions deterministically.
func waitForLockWaiters(t *testing.T, ctx context.Context, d *db.DB, n int) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var waiting int
		if err := d.Pool.QueryRow(ctx, `SELECT count(*) FROM pg_locks WHERE NOT granted AND locktype = 'relation'
			AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&waiting); err != nil {
			t.Fatal(err)
		}
		if waiting == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d lock requests wait, want %d", waiting, n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestLegacyWriterMigrationWithConcurrentCreates runs migration 0009 while
// legacy writers create organizations in other sessions: one transaction
// that began before the migration and commits while it waits for its lock,
// and one insert that arrives while the migration waits. Both organizations
// end with exactly one unlinked status row.
func TestLegacyWriterMigrationWithConcurrentCreates(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, legacyWriterMigration-1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	applied := appliedAt(t, ctx, d, billingStatusMigration)
	early, late := "018f0000-0000-7000-8000-0000000000e1", "018f0000-0000-7000-8000-0000000000e2"
	earlyCreate, lateCreate := applied.Add(-time.Hour), applied.Add(time.Hour)

	// Session 1: a legacy create whose transaction is still open.
	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, legacyOrg, early, earlyCreate); err != nil {
		t.Fatal(err)
	}

	// The migration waits for the lock behind the open insert.
	migrated := make(chan error, 1)
	go func() { migrated <- d.Migrate(ctx, testenv.Discard()) }()
	waitForLockWaiters(t, ctx, d, 1)

	// Session 2: a legacy create that arrives while the migration waits
	// queues behind the migration's lock request.
	created := make(chan error, 1)
	go func() {
		_, err := d.Pool.Exec(ctx, legacyOrg, late, lateCreate)
		created <- err
	}()
	waitForLockWaiters(t, ctx, d, 2)
	select {
	case err := <-migrated:
		t.Fatalf("the migration finished while a legacy create was open: %v", err)
	case err := <-created:
		t.Fatalf("a legacy create finished while the migration waited: %v", err)
	case <-time.After(200 * time.Millisecond):
	}

	// Session 1 commits: the migration backfills its organization, and the
	// waiting create runs after the migration with the trigger installed.
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-migrated; err != nil {
		t.Fatal(err)
	}
	if err := <-created; err != nil {
		t.Fatal(err)
	}
	wantBlocked(t, readBilling(t, ctx, d.Pool, early), applied)
	wantBlocked(t, readBilling(t, ctx, d.Pool, late), lateCreate)
	var orgs, rows int
	if err := d.Pool.QueryRow(ctx, "SELECT (SELECT count(*) FROM organizations), (SELECT count(*) FROM organization_billing)").Scan(&orgs, &rows); err != nil || orgs != 2 || rows != 2 {
		t.Fatalf("organizations = %d, status rows = %d, %v, want 2 each", orgs, rows, err)
	}
}
