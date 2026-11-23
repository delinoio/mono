// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

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
func exportRecoveryFixture(t *testing.T) (s *stack, c *ownsand.Client, ctx context.Context, snapID string, data []byte) {
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
	s.objects.waitPutHeld(t)
	waitExportState(ctx, t, c, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING)
	return s, c, ctx, snapID, data
}

// retryAndImport retries the export of a failed snapshot, checks that it
// reaches EXPORTED with its parts under one attempt prefix, and restores the
// snapshot on the second device with the original data.
func retryAndImport(ctx context.Context, t *testing.T, s *stack, c *ownsand.Client, snapID string, data []byte) {
	t.Helper()
	s.objects.releasePuts()
	if _, err := c.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("retry ExportSnapshot: %v", err)
	}
	waitExported(ctx, t, c, snapID)
	// A stale upload of the abandoned attempt may have stored objects under
	// its own prefix; the exported manifest points at exactly one attempt.
	attempts := s.exportAttempts(snapID)
	complete := 0
	var errs []error
	for id, parts := range attempts {
		if err := checkExportParts(parts, e2ePartSize, s.wantExportParts()); err != nil {
			errs = append(errs, fmt.Errorf("attempt %s: %w", id, err))
			continue
		}
		complete++
	}
	if complete < 1 {
		t.Fatalf("no complete export attempt: %v", errors.Join(errs...))
	}

	p := createParams()
	p.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
	p.SnapshotID = snapID
	p.DeviceID = s.agents[1].deviceID
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
	s, c, ctx, snapID, data := exportRecoveryFixture(t)
	s.killAgent(0)
	s.restartAgent(0)
	waitExportState(ctx, t, c, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	retryAndImport(ctx, t, s, c, snapID, data)
}

// TestSnapshotExportRecoversAfterControlPlaneRestart kills the control plane
// in the middle of an export and starts it again over the same database:
// the snapshot ends EXPORT_FAILED when the device reconnects, and an
// explicit retry exports it.
func TestSnapshotExportRecoversAfterControlPlaneRestart(t *testing.T) {
	s, c, ctx, snapID, data := exportRecoveryFixture(t)
	s.killAPI()
	s.restartAPI()
	waitExportState(ctx, t, c, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	retryAndImport(ctx, t, s, c, snapID, data)
}
