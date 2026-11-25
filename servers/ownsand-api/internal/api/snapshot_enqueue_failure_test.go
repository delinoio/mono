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

// enqueueFailureDeadline is the RPC deadline of the compensation tests that
// still end their request by a deadline. It is shorter than the queue-full
// wait of the saturated channel, so the request context ends first: the
// command send fails before enqueue and the channel stays open. Tests that
// need the committed state first cancel the request themselves instead (see
// failRPCAfterCommit).
const enqueueFailureDeadline = 100 * time.Millisecond

func snapshotStateOf(t *testing.T, c *testenv.Clients, orgID, snapshotID string) ownsandv1.SnapshotState {
	t.Helper()
	s, err := c.Snapshot.GetSnapshot(context.Background(), connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: orgID, SnapshotId: snapshotID}))
	if err != nil {
		t.Fatal(err)
	}
	return s.Msg.GetSnapshot().GetState()
}

// snapshotCreateFailsAfterCommit cancels the request only after the new
// snapshot is visible. The caller has already saturated the agent channel,
// so the cancellation fails the command send and starts compensation.
func snapshotCreateFailsAfterCommit(t *testing.T, c *testenv.Clients, orgID, sandboxID string) string {
	t.Helper()
	var snapID string
	failRPCAfterCommit(t, "CreateSnapshot",
		func(ctx context.Context) error {
			_, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: orgID, SandboxId: sandboxID}))
			return err
		},
		func() bool {
			list, err := c.Snapshot.ListSnapshots(t.Context(), connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: orgID}))
			if err != nil || len(list.Msg.GetSnapshots()) != 1 || list.Msg.GetSnapshots()[0].GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
				return false
			}
			snapID = list.Msg.GetSnapshots()[0].GetSnapshotId()
			return true
		})
	return snapID
}

// TestCreateSnapshotEnqueueFailureLeavesNoCreatingRow checks that a
// CreateSnapshot whose request ends before the command is queued does not
// leave the committed CREATING record behind: the record becomes FAILED
// even though the request context that caused the failure is done. The test
// cancels the request only after it observed the committed CREATING record,
// so the outcome does not depend on how fast the request commits.
func TestCreateSnapshotEnqueueFailureLeavesNoCreatingRow(t *testing.T) {
	env := testenv.New(t)
	env.App.Service.Hub.QueueFullWait = committedStateQueueFullWait
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
	snapID := snapshotCreateFailsAfterCommit(t, c, org.ID, sandboxID)

	testenv.Eventually(t, "the same snapshot is the only record", func() bool {
		list, err := c.Snapshot.ListSnapshots(ctx, connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: org.ID}))
		return err == nil && len(list.Msg.GetSnapshots()) == 1 && list.Msg.GetSnapshots()[0].GetSnapshotId() == snapID
	})
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
	env.App.Service.Hub.QueueFullWait = committedStateQueueFullWait
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
	exportFailsAfterClaim(t, c, org.ID, snapID)
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
