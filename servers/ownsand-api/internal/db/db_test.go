// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"testing"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
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

// deliveryIDMigration is the version of 0006_control_events_delivery_id.sql.
const deliveryIDMigration = 6

// TestDeliveryIDMigrationKeepsOldWritersWorking applies the migrations up to
// the one before the delivery ID migration, writes events the way the
// previous release does, applies the delivery ID migration and checks the
// backfill, the sequence and an event that an old instance inserts while a
// new instance polls (a rolling deployment).
func TestDeliveryIDMigrationKeepsOldWritersWorking(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.NewUnmigratedURL(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	if err := d.MigrateUpTo(ctx, testenv.Discard(), deliveryIDMigration-1); err != nil {
		t.Fatal(err)
	}
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
