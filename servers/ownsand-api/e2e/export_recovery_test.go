// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	ownsand "github.com/delinoio/mono/go/ownsand"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// waitExportState polls GetSnapshot until the export state is want.
func waitExportState(ctx context.Context, t *testing.T, c *ownsand.Client, id string, want ownsandv1.SnapshotExportState) {
	t.Helper()
	var last ownsandv1.SnapshotExportState
	for {
		res, err := c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{SnapshotId: id})
		if err == nil {
			last = res.GetSnapshot().GetExportState()
			if last == want {
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot %s export state = %v, want %v (last error %v)", id, last, want, err)
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// exportRecoveryFixture starts a stack with two devices, creates a snapshot
// of 12 MiB of data on the first and starts its export with the object
// store holding the part uploads, so the export is stuck mid-transfer.
func exportRecoveryFixture(t *testing.T) (s *stack, c *ownsand.Client, ctx context.Context, snapID, abandonedID string, data []byte) {
	t.Helper()
	s = newStack(t, stackOptions{agents: 2, objects: true})
	ctx = s.ctx(5 * time.Minute)
	c = s.apiKeyClient(ownsand.TransportRelay)
	p := createParams()
	p.DeviceID = s.agents[0].deviceID
	src, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	t.Cleanup(func() { src.Close() })
	if info, err := src.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable = (%v, %v)", info.GetState(), err)
	}
	data = randomBytes(t, 12<<20)
	if _, err := src.WriteFile(ctx, workDir(ctx, t, src)+"/data.bin", bytes.NewReader(data), ownsand.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snapID = snap.GetSnapshotId()
	waitSnapshotReady(ctx, t, c, snapID)
	s.objects.holdPuts()
	t.Cleanup(s.objects.releasePuts)
	if _, err := c.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	key := s.objects.waitPutHeld(t)
	rest, belongsToSnapshot := strings.CutPrefix(key, s.exportAttemptsPrefix(snapID))
	abandonedID, part, hasPart := strings.Cut(rest, "/")
	partNumber, err := strconv.Atoi(part)
	if !belongsToSnapshot || abandonedID == "" || !hasPart || err != nil || partNumber < 1 {
		t.Fatalf("held PUT key %q is not an export part of snapshot %s", key, snapID)
	}
	waitExportState(ctx, t, c, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING)
	return s, c, ctx, snapID, abandonedID, data
}

// retryExport retries the export once and checks that it reaches EXPORTED
// with a fresh attempt ID and a complete part set. It returns the part keys.
func retryExport(ctx context.Context, t *testing.T, s *stack, c *ownsand.Client, snapID, abandonedID string) []string {
	t.Helper()
	s.objects.releasePuts()
	if _, err := c.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("retry ExportSnapshot: %v", err)
	}
	waitExported(ctx, t, c, snapID)

	cfg, err := pgxpool.ParseConfig(s.databaseURL())
	if err != nil {
		t.Fatal("parse database configuration for export retry")
	}
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatalf("connect to the database: %v", err)
	}
	defer conn.Close(ctx)
	var retryID string
	err = conn.QueryRow(ctx, "SELECT COALESCE(export_command_id::text, '') FROM snapshots WHERE snapshot_id = $1", snapID).Scan(&retryID)
	if err != nil {
		t.Fatalf("read export command ID for snapshot %s: %v", snapID, err)
	}

	if err := checkRetryExportParts(s.exportAttempts(snapID), abandonedID, retryID, s.wantExportParts()); err != nil {
		t.Fatal(err)
	}
	return s.objects.keys(s.exportAttemptsPrefix(snapID) + retryID + "/")
}

func checkRetryExportParts(attempts map[string]map[int]int, abandonedID, retryID string, wantParts int) error {
	if retryID == "" {
		return fmt.Errorf("exported snapshot has no export command ID")
	}
	if retryID == abandonedID {
		return fmt.Errorf("export retry reused abandoned attempt %s", abandonedID)
	}
	// The abandoned attempt can still have objects under its own prefix.
	if err := checkExportParts(attempts[retryID], e2ePartSize, wantParts); err != nil {
		return fmt.Errorf("export retry attempt %s: %w", retryID, err)
	}
	return nil
}

func checkImportPartGets(got, want []string) error {
	seen := make(map[string]bool, len(want))
	for _, key := range want {
		seen[key] = false
	}
	for _, key := range got {
		if _, ok := seen[key]; !ok {
			return fmt.Errorf("import requested unexpected object %q", key)
		}
		seen[key] = true
	}
	for _, key := range want {
		if !seen[key] {
			return fmt.Errorf("import did not request retry part %q", key)
		}
	}
	return nil
}

// importExport restores the snapshot on the second device and checks the
// requests for the retry's parts and the restored bytes.
func importExport(ctx context.Context, t *testing.T, s *stack, c *ownsand.Client, snapID string, partKeys []string, data []byte) {
	t.Helper()
	p := createParams()
	p.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
	p.SnapshotID = snapID
	p.DeviceID = s.agents[1].deviceID
	position := s.objects.getPosition()
	restored, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox on the second device: %v", err)
	}
	defer restored.Close()
	info, err := restored.WaitStable(ctx)
	if err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("restored WaitStable = (%v, %v)", info.GetState(), err)
	}
	// The import must restore the snapshot on the requested second device;
	// a sandbox placed on the source device would satisfy every other check.
	if info.GetDeviceId() != s.agents[1].deviceID {
		t.Fatalf("restored sandbox device = %q, want the second device %q", info.GetDeviceId(), s.agents[1].deviceID)
	}
	if err := checkImportPartGets(s.objects.getsSince(position), partKeys); err != nil {
		t.Fatal(err)
	}
	got, err := restored.ReadFileBytes(ctx, workDir(ctx, t, restored)+"/data.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored ReadFile = (%d bytes, %v)", len(got), err)
	}
}

// TestSnapshotExportRecoversAfterAgentRestart kills the agent in the middle
// of an export and starts it again: the snapshot ends EXPORT_FAILED when
// the device reports on its new channel, and an explicit retry exports it
// and an import on a second device restores the data.
func TestSnapshotExportRecoversAfterAgentRestart(t *testing.T) {
	t.Parallel()
	s, c, ctx, snapID, abandonedID, data := exportRecoveryFixture(t)
	sourceID := s.agents[0].deviceID
	position := s.logs.position()
	s.killAgent(t, 0)
	// The API process is unchanged. Use the disconnect to exclude reports
	// from the channel that carried the abandoned export.
	position = s.waitForAgentChannelEvent(ctx, sourceID, agentChannelDisconnected, position)
	s.restartAgent(t, 0)
	waitExportState(ctx, t, c, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	s.waitForAgentChannelEvent(ctx, sourceID, agentStateReport, position)
	partKeys := retryExport(ctx, t, s, c, snapID, abandonedID)
	importExport(ctx, t, s, c, snapID, partKeys, data)
}

// TestSnapshotExportRecoversAfterControlPlaneRestart kills the control plane
// in the middle of an export and starts it again over the same database:
// the snapshot ends EXPORT_FAILED when the device reconnects, and an
// explicit retry exports it.
func TestSnapshotExportRecoversAfterControlPlaneRestart(t *testing.T) {
	t.Parallel()
	s, c, ctx, snapID, abandonedID, data := exportRecoveryFixture(t)
	s.killAPI(t)
	position := s.logs.position()
	s.restartAPI(t)
	waitExportState(ctx, t, c, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	s.waitForAgentChannelEvent(ctx, s.agents[0].deviceID, agentStateReport, position)
	partKeys := retryExport(ctx, t, s, c, snapID, abandonedID)
	s.waitForAgentChannelEvent(ctx, s.agents[1].deviceID, agentStateReport, position)
	importExport(ctx, t, s, c, snapID, partKeys, data)
}

func TestCheckRetryExportParts(t *testing.T) {
	t.Parallel()
	complete := map[int]int{1: e2ePartSize, 2: e2ePartSize, 3: 1}
	for _, tc := range []struct {
		name     string
		retryID  string
		attempts map[string]map[int]int
		wantErr  string
	}{
		{
			name:     "complete abandoned attempt alone",
			retryID:  "retry",
			attempts: map[string]map[int]int{"abandoned": complete},
			wantErr:  "export retry attempt retry: 0 parts, want 3",
		},
		{
			name:     "reused attempt ID",
			retryID:  "abandoned",
			attempts: map[string]map[int]int{"abandoned": complete},
			wantErr:  "export retry reused abandoned attempt abandoned",
		},
		{
			name:     "missing attempt ID",
			attempts: map[string]map[int]int{"abandoned": complete},
			wantErr:  "exported snapshot has no export command ID",
		},
		{
			name:     "complete retry beside abandoned attempt",
			retryID:  "retry",
			attempts: map[string]map[int]int{"abandoned": complete, "retry": complete},
		},
	} {
		err := checkRetryExportParts(tc.attempts, "abandoned", tc.retryID, 3)
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("%s: %v", tc.name, err)
			}
		} else if err == nil || err.Error() != tc.wantErr {
			t.Errorf("%s: error = %v, want %q", tc.name, err, tc.wantErr)
		}
	}
}
