// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
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
