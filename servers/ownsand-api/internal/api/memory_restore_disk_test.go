// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestMemoryRestoreNeedsSnapshotDiskSize checks that the persisted disk size
// of a snapshot reaches placement for a snapshot source and a template
// source: a create with the same vCPUs, memory, network mode and disk size
// may restore the memory state, a create with a larger disk may not (the agent
// boots such a sandbox cold), and a create with a smaller disk stays an
// invalid argument.
func TestMemoryRestoreNeedsSnapshotDiskSize(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	_, agent := setupAgent(t, env, org, &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 32768, DiskGib: 1000, VcpuOvercommitPercent: 100})
	c := org.OwnerClient
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: created.Msg.GetSandbox().GetSandboxId()}))
	if err != nil {
		t.Fatal(err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	testenv.Eventually(t, "snapshot ready with memory state", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_READY && s.Msg.GetSnapshot().GetHasMemoryState()
	})
	tmpl, err := c.Snapshot.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{OrganizationId: org.ID, SnapshotId: snapID, Name: "base"}))
	if err != nil {
		t.Fatal(err)
	}
	sources := map[string]*ownsandv1.SandboxSource{
		"snapshot": {Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}},
		"template": {Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: tmpl.Msg.GetTemplate().GetTemplateId()}},
	}
	// createFrom returns whether the create command allows a memory restore.
	createFrom := func(source *ownsandv1.SandboxSource, diskGiB int64) bool {
		t.Helper()
		req := createReq(org.ID)
		req.Source = source
		req.Resources.DiskGib = diskGiB
		resp, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("create with disk %d: %v", diskGiB, err)
		}
		id := resp.Msg.GetSandbox().GetSandboxId()
		cmd := agent.WaitFor("create command", func(e *ownsandv1.AgentEnvelope) bool {
			return e.GetCommand().GetCreateSandbox().GetSandboxId() == id
		}).GetCommand().GetCreateSandbox()
		return cmd.GetMemoryRestoreAllowed()
	}
	for name, source := range sources {
		if !createFrom(source, 10) {
			t.Fatalf("%s: equal resources did not allow the memory restore", name)
		}
		if createFrom(source, 20) {
			t.Fatalf("%s: a larger disk allowed the memory restore", name)
		}
		req := createReq(org.ID)
		req.Source = source
		req.Resources.DiskGib = 5
		if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req)); testenv.Code(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: smaller disk: %v", name, err)
		}
	}
}
