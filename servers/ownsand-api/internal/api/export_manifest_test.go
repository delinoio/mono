// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"strconv"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func countManifestInserts(t *testing.T, env *testenv.Env) func() int64 {
	t.Helper()
	ctx := context.Background()
	for _, stmt := range []string{
		"CREATE SEQUENCE manifest_inserts",
		`CREATE FUNCTION count_manifest_insert() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN
				PERFORM nextval('manifest_inserts');
				RETURN NULL;
			END $$`,
		"CREATE TRIGGER manifest_insert AFTER INSERT ON snapshot_parts FOR EACH STATEMENT EXECUTE FUNCTION count_manifest_insert()",
	} {
		if _, err := env.DB.Pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	// PostgreSQL does not roll back sequence increments, so this also counts
	// inserts that succeeded before a later statement rolled back the transaction.
	return func() int64 {
		t.Helper()
		var n int64
		if err := env.DB.Pool.QueryRow(ctx, "SELECT CASE WHEN is_called THEN last_value ELSE 0 END FROM manifest_inserts").Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
}

func TestExportResultWritesManifestInOneStatement(t *testing.T) {
	for _, tc := range []struct {
		name        string
		partCount   int
		rejectState bool
	}{
		{name: "single part", partCount: 1},
		{name: "part limit", partCount: 10000},
		{name: "rollback", partCount: 1, rejectState: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			env := testenv.New(t)
			org := env.NewOrg("alice")
			snapID, agent := exportingSnapshot(t, env, org)
			command := waitExportCommands(t, agent, 1)[0]
			oldHash := sha256.Sum256([]byte("old part"))
			if _, err := env.DB.Pool.Exec(ctx,
				"INSERT INTO snapshot_parts (snapshot_id, part_number, size_bytes, sha256) VALUES ($1, 2, 7, $2)", snapID, oldHash[:]); err != nil {
				t.Fatal(err)
			}
			inserts := countManifestInserts(t, env)
			if tc.rejectState {
				// Reject only the update that schedules verification, which
				// follows the insert in the export result transaction.
				for _, stmt := range []string{
					`CREATE FUNCTION reject_export_update() RETURNS trigger LANGUAGE plpgsql AS $$
						BEGIN
							RAISE EXCEPTION 'injected export state update failure';
						END $$`,
					"CREATE TRIGGER reject_export_update BEFORE UPDATE OF export_verify_after ON snapshots FOR EACH ROW EXECUTE FUNCTION reject_export_update()",
				} {
					if _, err := env.DB.Pool.Exec(ctx, stmt); err != nil {
						t.Fatal(err)
					}
				}
			}

			parts := make([]*ownsandv1.SnapshotPart, tc.partCount)
			for i := range parts {
				n := tc.partCount - i
				hash := sha256.Sum256([]byte(strconv.Itoa(n)))
				parts[i] = &ownsandv1.SnapshotPart{PartNumber: int32(n), SizeBytes: int64(n) << 20, Sha256: hash[:]}
			}
			sendExportResult(agent, command, parts)

			wantState := ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
			wantVerifyAfter := env.Clock.Now().Add(15 * time.Minute)
			if tc.rejectState {
				waitLog(t, env, "injected export state update failure")
				wantState = ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING
				wantVerifyAfter = time.Time{}
				parts = []*ownsandv1.SnapshotPart{{PartNumber: 2, SizeBytes: 7, Sha256: oldHash[:]}}
			} else {
				waitExportState(t, org.OwnerClient, org.ID, snapID, wantState)
			}

			var state int16
			var verified bool
			var verifyAfter *time.Time
			if err := env.DB.Pool.QueryRow(ctx, "SELECT export_state, export_verified, export_verify_after FROM snapshots WHERE snapshot_id = $1", snapID).
				Scan(&state, &verified, &verifyAfter); err != nil {
				t.Fatal(err)
			}
			if state != int16(wantState) || verified ||
				(verifyAfter == nil) != wantVerifyAfter.IsZero() ||
				(verifyAfter != nil && !verifyAfter.Equal(wantVerifyAfter)) {
				t.Fatalf("export state = %d, verified = %t, verify after = %v, want %v, false, %v", state, verified, verifyAfter, wantState, wantVerifyAfter)
			}

			rows, err := env.DB.Pool.Query(ctx, "SELECT part_number, size_bytes, sha256 FROM snapshot_parts WHERE snapshot_id = $1 ORDER BY part_number DESC", snapID)
			if err != nil {
				t.Fatal(err)
			}
			defer rows.Close()
			i := 0
			for ; rows.Next(); i++ {
				var number int32
				var size int64
				var hash []byte
				if err := rows.Scan(&number, &size, &hash); err != nil {
					t.Fatal(err)
				}
				if i >= len(parts) || number != parts[i].GetPartNumber() || size != parts[i].GetSizeBytes() || !bytes.Equal(hash, parts[i].GetSha256()) {
					t.Fatalf("unexpected manifest part %d at row %d", number, i+1)
				}
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			if i != len(parts) {
				t.Fatalf("manifest rows = %d, want %d", i, len(parts))
			}
			if n := inserts(); n != 1 {
				t.Fatalf("manifest insert statements = %d, want 1", n)
			}
			t.Logf("manifest insert statements = 1, committed rows = %d, state = %s, verification scheduled = %t", i, wantState, verifyAfter != nil)
		})
	}
}
