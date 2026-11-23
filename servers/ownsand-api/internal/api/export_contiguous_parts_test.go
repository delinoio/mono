// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/sha256"
	"fmt"
	"slices"
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestExportPartNumbersMustBeContiguous checks that a result whose part
// numbers do not form the set 1..N for the N reported parts (an interior gap,
// or a first number above 1) fails the attempt as an invalid part with a
// clean-up job and no stored manifest, that a retry of the same snapshot is
// still possible, and that a complete manifest in any reported order reaches
// EXPORTED with its parts stored in ascending order.
func TestExportPartNumbersMustBeContiguous(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, agent := exportingSnapshot(t, env, org)

	part := func(n int32) *ownsandv1.SnapshotPart {
		// The hash encodes the number, so the stored order is observable.
		sum := sha256.Sum256([]byte(fmt.Sprintf("part-%d", n)))
		return &ownsandv1.SnapshotPart{PartNumber: n, SizeBytes: 1 << 20, Sha256: sum[:]}
	}
	numbered := func(numbers ...int32) []*ownsandv1.SnapshotPart {
		parts := make([]*ownsandv1.SnapshotPart, 0, len(numbers))
		for _, n := range numbers {
			parts = append(parts, part(n))
		}
		return parts
	}

	rejected := []struct {
		name    string
		numbers []int32
	}{
		{"interior gap", []int32{1, 3}},
		{"first number above one", []int32{2, 3}},
		{"single part above one", []int32{2}},
		{"gap in an unordered result", []int32{4, 1, 2}},
	}
	for i, tc := range rejected {
		ids := waitExportCommands(t, agent, i+1)
		command := ids[i]
		sendExportResult(agent, command, numbered(tc.numbers...))
		waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
		logs := env.Logs.String()
		if !strings.Contains(logs, `"command_id":"`+command+`"`) || !strings.Contains(logs, `"reason":"invalid_part"`) {
			t.Fatalf("%s: no export_failed log with reason invalid_part for command %s", tc.name, command)
		}
		if _, ok := cleanupJobs(t, env)[attemptPrefixOf(org.ID, snapID, command)]; !ok {
			t.Fatalf("%s: no clean-up job for the failed attempt: %v", tc.name, cleanupJobs(t, env))
		}
		if got := manifestHashes(t, env, snapID); len(got) != 0 {
			t.Fatalf("%s: the failed attempt stored %d parts", tc.name, len(got))
		}
		if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
			t.Fatalf("%s: retry state = %v", tc.name, got)
		}
	}

	// The last retry is pending: a complete manifest in descending order
	// completes the export and is stored in ascending order.
	command := waitExportCommands(t, agent, len(rejected)+1)[len(rejected)]
	sendExportResult(agent, command, numbered(3, 1, 2))
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	rows, err := env.DB.Pool.Query(context.Background(), "SELECT part_number FROM snapshot_parts WHERE snapshot_id = $1 ORDER BY part_number", snapID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var stored []int32
	for rows.Next() {
		var n int32
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		stored = append(stored, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := []int32{1, 2, 3}; !slices.Equal(stored, want) {
		t.Fatalf("stored part numbers = %v, want %v", stored, want)
	}
	hashes := manifestHashes(t, env, snapID)
	for i, h := range hashes {
		if want := part(int32(i + 1)).GetSha256(); string(h) != string(want) {
			t.Fatalf("stored part %d has the hash of another part", i+1)
		}
	}
}
