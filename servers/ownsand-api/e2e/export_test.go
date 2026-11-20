// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"bytes"
	"context"
	"testing"
	"time"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func waitExported(ctx context.Context, t *testing.T, c *ownsand.Client, id string) *ownsandv1.Snapshot {
	t.Helper()
	for {
		res, err := c.Snapshots().Get(ctx, &ownsandv1.GetSnapshotRequest{SnapshotId: id})
		if err != nil {
			t.Fatalf("GetSnapshot: %v", err)
		}
		switch res.GetSnapshot().GetExportState() {
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED:
			return res.GetSnapshot()
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED:
			t.Fatalf("snapshot %s export failed", id)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("snapshot %s was not exported: %v", id, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// TestSnapshotExportImport exports a snapshot from one device to the fake
// S3 store in several parts, creates a sandbox from it on a second device
// (which imports it with the hashes from the control plane) and deletes the
// snapshot, which removes both local copies.
func TestSnapshotExportImport(t *testing.T) {
	s := newStack(t, stackOptions{agents: 2, objects: true})
	ctx := s.ctx(5 * time.Minute)
	c := s.apiKeyClient(ownsand.TransportRelay)
	a, b := s.agents[0], s.agents[1]

	p := createParams()
	p.DeviceID = a.deviceID
	src, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox: %v", err)
	}
	defer src.Close()
	if info, err := src.WaitStable(ctx); err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		t.Fatalf("WaitStable = (%v, %v)", info.GetState(), err)
	}
	// 12 MiB of random data makes a tar stream of three 5 MiB parts.
	data := randomBytes(t, 12<<20)
	if _, err := src.WriteFile(ctx, workDir(ctx, t, src)+"/data.bin", bytes.NewReader(data), ownsand.WriteOptions{}); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	snap, err := src.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	snapID := snap.GetSnapshotId()
	waitSnapshotReady(ctx, t, c, snapID)
	if _, err := c.Snapshots().Export(ctx, &ownsandv1.ExportSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("ExportSnapshot: %v", err)
	}
	waitExported(ctx, t, c, snapID)
	keys := s.objects.keys("organizations/" + s.orgID + "/snapshots/" + snapID + "/parts/")
	if len(keys) != 3 {
		t.Fatalf("exported objects %v, want 3 parts", keys)
	}

	p = createParams()
	p.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED
	p.SnapshotID = snapID
	p.DeviceID = b.deviceID
	restored, err := c.CreateSandbox(ctx, p)
	if err != nil {
		t.Fatalf("CreateSandbox on the second device: %v", err)
	}
	defer restored.Close()
	info, err := restored.WaitStable(ctx)
	if err != nil || info.GetState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || info.GetDeviceId() != b.deviceID {
		t.Fatalf("restored WaitStable = (%v on %s, %v)", info.GetState(), info.GetDeviceId(), err)
	}
	got, err := restored.ReadFileBytes(ctx, workDir(ctx, t, restored)+"/data.bin")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("restored ReadFile = (%d bytes, %v)", len(got), err)
	}
	for _, sb := range []*ownsand.Sandbox{restored, src} {
		if _, err := sb.Delete(ctx); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if _, err := c.Snapshots().Delete(ctx, &ownsandv1.DeleteSnapshotRequest{SnapshotId: snapID}); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	s.waitLocalSnapshotGone(t, a, snapID)
	s.waitLocalSnapshotGone(t, b, snapID)
}
