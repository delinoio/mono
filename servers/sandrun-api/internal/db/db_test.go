// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"regexp"
	"testing"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db/dbtest"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestOpenInvalidDatabaseURL(t *testing.T) {
	database, err := db.Open(t.Context(), "postgres://[")
	if database != nil {
		database.Close()
		t.Fatal("Open returned a database for an invalid URL")
	}

	const want = "parse database url: invalid connection string"
	if err == nil || err.Error() != want {
		t.Fatalf("Open error = %v, want %q", err, want)
	}
	if errors.Unwrap(err) != nil {
		t.Fatal("Open error wraps the parser error")
	}
}

// TestMigrationNames checks the naming convention of the embedded files.
func TestMigrationNames(t *testing.T) {
	names, err := fs.Glob(db.Migrations(), "*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v %v", names, err)
	}
	pattern := regexp.MustCompile(`^[0-9]{5}_[a-z0-9_]+\.sql$`)
	for _, name := range names {
		if !pattern.MatchString(name) {
			t.Errorf("%s does not match <5-digit version>_<name>.sql", name)
		}
	}
}

// TestMigrateTwice checks that a second run on a migrated database applies
// nothing and that the core tables exist.
func TestMigrateTwice(t *testing.T) {
	d := dbtest.New(t)
	ctx := context.Background()
	if err := d.Migrate(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"organizations", "memberships", "invitations", "api_keys", "audit_events", "instance_leases"} {
		var exists bool
		if err := d.Pool.QueryRow(ctx, "SELECT to_regclass($1) IS NOT NULL", table).Scan(&exists); err != nil || !exists {
			t.Errorf("table %s: %v %v", table, exists, err)
		}
	}
}

func TestMigrateRefusesBeforeAnyUpgradeDDL(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 7))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if err := d.Migrate(ctx, logger); !errors.Is(err, db.ErrQuiescedV9UpgradeRequired) {
		t.Fatalf("normal migration error = %v, want maintenance refusal", err)
	}
	assertWatchTransactionMigrationPending(t, d, 7)
}

// TestWatchQueueTransactionMigration runs the production migration over rows
// created with the old trigger and then writes through the old SQL shape.
func TestWatchQueueTransactionMigration(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 8))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, err = d.Pool.Exec(ctx, `
		INSERT INTO organizations (organization_id, name, create_time)
		VALUES ('00000000-0000-0000-0000-000000000001', 'legacy', now());
		INSERT INTO requests (request_id, organization_id, runner_profile_id, kind, source, source_key,
			state, priority, queue_time, queue_deadline, create_time, update_time)
		VALUES (gen_random_uuid(), '00000000-0000-0000-0000-000000000001', gen_random_uuid(), 3, 1,
			'legacy', 3, 2, now(), now(), now(), now());
		INSERT INTO runners (runner_id, organization_id, runner_profile_id, kind, guest_os, state,
			idempotency_key, create_request, create_time, update_time)
		VALUES (gen_random_uuid(), '00000000-0000-0000-0000-000000000001', gen_random_uuid(),
			3, 1, 5, gen_random_uuid(), ''::bytea, now(), now());`)
	if err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	// Make the last statement of v9 fail after both ALTER TABLEs, the
	// trigger function change and the first index have run.
	if _, err := d.Pool.Exec(ctx, "CREATE INDEX runners_watch_xact_idx ON organizations (organization_id)"); err != nil {
		t.Fatal(err)
	}
	var pgErr *pgconn.PgError
	if err := d.MigrateQuiescedV9(ctx, logger); !errors.As(err, &pgErr) || pgErr.Code != "42P07" {
		t.Fatalf("quiesced migration error = %v, want duplicate relation", err)
	}
	assertWatchTransactionMigrationPending(t, d, 8)
	if _, err := d.Pool.Exec(ctx, "DROP INDEX runners_watch_xact_idx"); err != nil {
		t.Fatal(err)
	}

	if err := d.MigrateQuiescedV9(ctx, logger); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, logger); err != nil {
		t.Fatal(err)
	}
	var applied int
	if err := d.Pool.QueryRow(ctx, "SELECT count(*) FROM goose_db_version WHERE version_id = 9 AND is_applied").Scan(&applied); err != nil || applied != 1 {
		t.Fatalf("applied v9 records = %d, error = %v, want one record", applied, err)
	}
	var indexed int
	if err := d.Pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes WHERE schemaname = current_schema()
		AND (tablename, indexname) IN (('requests', 'requests_watch_xact_idx'), ('runners', 'runners_watch_xact_idx'))
	`).Scan(&indexed); err != nil || indexed != 2 {
		t.Fatalf("v9 watch indexes = %d, error = %v, want two indexes", indexed, err)
	}

	tx, err := d.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var xid string
	if err := tx.QueryRow(ctx, "SELECT pg_current_xact_id()::text").Scan(&xid); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"requests", "runners"} {
		var originalSeq int64
		var unstamped bool
		if err := tx.QueryRow(ctx, fmt.Sprintf("SELECT change_seq, watch_xact_id IS NULL FROM %s", table)).Scan(&originalSeq, &unstamped); err != nil {
			t.Fatal(err)
		}
		if !unstamped || originalSeq == 0 {
			t.Fatalf("legacy %s row has sequence %d and unstamped=%t", table, originalSeq, unstamped)
		}
		var seq int64
		var stamp string
		if err := tx.QueryRow(ctx, fmt.Sprintf("UPDATE %s SET update_time = now() RETURNING change_seq, watch_xact_id::text", table)).Scan(&seq, &stamp); err != nil {
			t.Fatal(err)
		}
		if stamp != xid || seq <= originalSeq {
			t.Fatalf("%s write stamped transaction %s and sequence %d, want transaction %s and sequence greater than %d", table, stamp, seq, xid, originalSeq)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func assertWatchTransactionMigrationPending(t *testing.T, d *db.DB, wantVersion int64) {
	t.Helper()
	var version int64
	var columns, records int
	if err := d.Pool.QueryRow(t.Context(), `
		SELECT max(version_id), count(*) FILTER (WHERE version_id = 9),
			(SELECT count(*) FROM information_schema.columns WHERE table_schema = current_schema()
			 AND table_name IN ('requests', 'runners') AND column_name = 'watch_xact_id')
		FROM goose_db_version
	`).Scan(&version, &records, &columns); err != nil || version != wantVersion || records != 0 || columns != 0 {
		t.Fatalf("schema version = %d, v9 records = %d, columns = %d, error = %v, want version %d with v9 pending",
			version, records, columns, err, wantVersion)
	}
}

// TestOrganizationDeletionIntentMigration verifies that existing link
// removal claims keep their original owner after the forward migration.
func TestOrganizationDeletionIntentMigration(t *testing.T) {
	ctx := t.Context()
	d, err := db.Open(ctx, dbtest.NewURLAtVersion(t, 11))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	const orgID = "00000000-0000-0000-0000-000000000001"
	if _, err := d.Pool.Exec(ctx, `
		INSERT INTO organizations (organization_id, name, create_time)
		VALUES ('00000000-0000-0000-0000-000000000001', 'legacy', now());
		INSERT INTO ownsand_links (organization_id, ownsand_link_id, ownsand_organization_id,
			state, removing, api_key_key_id, api_key_ciphertext, credential_set_time, create_time)
		VALUES ('00000000-0000-0000-0000-000000000001', gen_random_uuid(), gen_random_uuid(),
			1, true, 'fixture', '', now(), now());`); err != nil {
		t.Fatal(err)
	}
	if err := d.Migrate(ctx, slog.New(slog.NewJSONHandler(io.Discard, nil))); err != nil {
		t.Fatal(err)
	}
	var removing bool
	var orgAttempt, linkAttempt *string
	if err := d.Pool.QueryRow(ctx, `
		SELECT l.removing, o.deletion_attempt_id::text, l.organization_deletion_attempt_id::text
		FROM organizations o JOIN ownsand_links l USING (organization_id) WHERE o.organization_id = $1`,
		orgID).Scan(&removing, &orgAttempt, &linkAttempt); err != nil {
		t.Fatal(err)
	}
	if !removing || orgAttempt != nil || linkAttempt != nil {
		t.Fatalf("migration changed a standalone link claim: removing = %v, organization attempt = %v, link attempt = %v", removing, orgAttempt, linkAttempt)
	}
	t.Log("Forward migration preserved the existing standalone link removal claim and left both attempt IDs unset.")
}
