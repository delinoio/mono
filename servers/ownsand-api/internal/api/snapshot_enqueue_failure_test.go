// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// enqueueFailureDeadline is the RPC deadline of these tests. It is shorter
// than the queue-full wait of the saturated channel, so the request context
// ends first: the command send fails before enqueue and the channel stays
// open.
const enqueueFailureDeadline = 100 * time.Millisecond

func snapshotStateOf(t *testing.T, c *testenv.Clients, orgID, snapshotID string) ownsandv1.SnapshotState {
	t.Helper()
	s, err := c.Snapshot.GetSnapshot(context.Background(), connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: orgID, SnapshotId: snapshotID}))
	if err != nil {
		t.Fatal(err)
	}
	return s.Msg.GetSnapshot().GetState()
}

// TestCreateSnapshotEnqueueFailureLeavesNoCreatingRow checks that a
// CreateSnapshot whose request ends before the command is queued does not
// leave the committed CREATING record behind: the record becomes FAILED
// even though the request context that caused the failure is done.
func TestCreateSnapshotEnqueueFailureLeavesNoCreatingRow(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	_, agent := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)

	saturate(t, env, agent)
	rpcCtx, cancel := context.WithTimeout(ctx, enqueueFailureDeadline)
	defer cancel()
	if _, err := c.Snapshot.CreateSnapshot(rpcCtx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err == nil {
		t.Fatal("CreateSnapshot on a saturated channel succeeded")
	}

	list, err := c.Snapshot.ListSnapshots(ctx, connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: org.ID}))
	if err != nil {
		t.Fatal(err)
	}
	if n := len(list.Msg.GetSnapshots()); n != 1 {
		t.Fatalf("snapshots = %d, want the one record of the failed attempt", n)
	}
	snapID := list.Msg.GetSnapshots()[0].GetSnapshotId()
	testenv.Eventually(t, "snapshot FAILED", func() bool {
		return snapshotStateOf(t, c, org.ID, snapID) == ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED
	})
}

// TestExportSnapshotEnqueueFailureAllowsRetry checks that an ExportSnapshot
// whose request ends before the command is queued ends the attempt as
// EXPORT_FAILED, so a later call on the same live channel starts a new
// attempt instead of returning the stranded EXPORTING state.
func TestExportSnapshotEnqueueFailureAllowsRetry(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = saturationQueueFullWait
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	_, agent := setupAgent(t, env, org, nil)
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snapID := plainSnapshot(t, org, sandboxID)

	saturate(t, env, agent)
	rpcCtx, cancel := context.WithTimeout(ctx, enqueueFailureDeadline)
	defer cancel()
	if _, err := c.Snapshot.ExportSnapshot(rpcCtx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err == nil {
		t.Fatal("ExportSnapshot on a saturated channel succeeded")
	}
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)
	if ids := exportCommandIDs(agent); len(ids) != 0 {
		t.Fatalf("export commands before the retry = %v", ids)
	}

	// The channel is still open: the agent reads again and the retry runs.
	agent.ResumeReads()
	if got := exportSnapshotRPC(t, c, org.ID, snapID); got != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
		t.Fatalf("retry state = %v", got)
	}
	waitExportState(t, c, org.ID, snapID, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)
	if ids := exportCommandIDs(agent); len(ids) != 1 {
		t.Fatalf("export commands after the retry = %v, want exactly one", ids)
	}
}
