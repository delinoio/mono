// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// remoteBlockedSinceMigration is the version of
// 0016_billing_remote_blocked_since.sql.
const remoteBlockedSinceMigration = 16

// TestRemoteBlockedSinceMigrationKeepsStatusRows upgrades a database at the
// schema of migration 0015 that holds a linked BLOCKED status row, and
// checks that the row survives unchanged, that the new column starts NULL
// and that the status write of the previous release, which does not know
// the column, still works and leaves it NULL.
func TestRemoteBlockedSinceMigrationKeepsStatusRows(t *testing.T) {
	ctx := context.Background()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, remoteBlockedSinceMigration-1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	const (
		org     = "018f0000-0000-7000-8000-0000000000e2"
		account = "018f0000-0000-7000-8000-0000000000a2"
	)
	since := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	if _, err := d.Pool.Exec(ctx, legacyOrg, org, since); err != nil {
		t.Fatal(err)
	}
	// The status row of a linked organization in non-payment, written like
	// the previous release does.
	const oldUpdate = `UPDATE organization_billing SET billing_account_id = $2, phase = 2, reason = 2, blocked_since = $3, suspend_time = $4,
		status_sync_time = $3, status_sequence = 5, update_time = $3 WHERE organization_id = $1`
	if _, err := d.Pool.Exec(ctx, oldUpdate, org, account, since, since.Add(72*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, testenv.Discard()); err != nil {
		t.Fatal(err)
	}
	var remote *time.Time
	var blocked *time.Time
	var sequence int64
	const read = "SELECT remote_blocked_since, blocked_since, status_sequence FROM organization_billing WHERE organization_id = $1"
	if err := d.Pool.QueryRow(ctx, read, org).Scan(&remote, &blocked, &sequence); err != nil {
		t.Fatal(err)
	}
	if remote != nil || blocked == nil || !blocked.Equal(since) || sequence != 5 {
		t.Fatalf("after the upgrade: remote_blocked_since %v, blocked_since %v, sequence %d; want NULL, %v, 5", remote, blocked, sequence, since)
	}
	// The previous release writes the row with its old column list.
	if _, err := d.Pool.Exec(ctx, oldUpdate, org, account, since.Add(time.Hour), since.Add(73*time.Hour)); err != nil {
		t.Fatalf("status write of the previous release: %v", err)
	}
	if err := d.Pool.QueryRow(ctx, read, org).Scan(&remote, &blocked, &sequence); err != nil || remote != nil || !blocked.Equal(since.Add(time.Hour)) {
		t.Fatalf("after the old write: remote_blocked_since %v, blocked_since %v, %v; want NULL, %v", remote, blocked, err, since.Add(time.Hour))
	}
}
