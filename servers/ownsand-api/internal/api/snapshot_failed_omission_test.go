// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// appliedReports counts the agent_state_report log lines of deviceID.
func appliedReports(env *testenv.Env, deviceID string) int {
	n := 0
	for _, line := range bytes.Split([]byte(env.Logs.String()), []byte("\n")) {
		if !bytes.Contains(line, []byte(`"agent_state_report"`)) {
			continue
		}
		var rec struct {
			DeviceID string `json:"device_id"`
		}
		if json.Unmarshal(line, &rec) == nil && rec.DeviceID == deviceID {
			n++
		}
	}
	return n
}

// sendReportApplied sends a state report of agent and waits until the
// server logged one more applied report of the device.
func sendReportApplied(t *testing.T, env *testenv.Env, agent *testenv.FakeAgent) {
	t.Helper()
	before := appliedReports(env, agent.Device.ID)
	agent.SendStateReport()
	testenv.Eventually(t, "state report applied", func() bool { return appliedReports(env, agent.Device.ID) > before })
}

func getSnapshotErr(c *testenv.Clients, orgID, snapshotID string) (*ownsandv1.Snapshot, error) {
	res, err := c.Snapshot.GetSnapshot(context.Background(), connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: orgID, SnapshotId: snapshotID}))
	if err != nil {
		return nil, err
	}
	return res.Msg.GetSnapshot(), nil
}

// TestStateReportOmittingCreatingSnapshotKeepsFailedRecord checks that a
// committed CREATING snapshot that the agent never reports becomes FAILED at
// the first report that omits it and stays FAILED, with the same ID, at the
// following reports that omit it.
func TestStateReportOmittingCreatingSnapshotKeepsFailedRecord(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	_, agent := setupAgent(t, env, org, nil)
	// The agent never answers a snapshot command, so the record stays in
	// CREATING and no report lists it.
	agent.SetOnCommand(func(a *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		if cmd.GetCreateSnapshot() != nil {
			return nil
		}
		return a.DefaultAnswer(cmd)
	})
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(createReq(org.ID)))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snap, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
	if err != nil {
		t.Fatal(err)
	}
	snapID := snap.Msg.GetSnapshot().GetSnapshotId()
	if got := snapshotStateOf(t, c, org.ID, snapID); got != ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING {
		t.Fatalf("state before the reports = %v, want CREATING", got)
	}

	for i := 1; i <= 3; i++ {
		sendReportApplied(t, env, agent)
		got, err := getSnapshotErr(c, org.ID, snapID)
		if err != nil {
			t.Fatalf("GetSnapshot after omitting report %d: %v", i, err)
		}
		if got.GetSnapshotId() != snapID || got.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED {
			t.Fatalf("after omitting report %d: id = %s state = %v, want %s FAILED", i, got.GetSnapshotId(), got.GetState(), snapID)
		}
	}
}

// TestStateReportKeepsFailedCreateRecord checks that the FAILED record that
// the compensation of a CreateSnapshot whose command never reached the agent
// leaves behind survives a state report that omits it.
func TestStateReportKeepsFailedCreateRecord(t *testing.T) {
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
	var snapID string
	failRPCAfterCommit(t, "CreateSnapshot",
		func(ctx context.Context) error {
			_, err := c.Snapshot.CreateSnapshot(ctx, connect.NewRequest(&ownsandv1.CreateSnapshotRequest{OrganizationId: org.ID, SandboxId: sandboxID}))
			return err
		},
		func() bool {
			list, err := c.Snapshot.ListSnapshots(ctx, connect.NewRequest(&ownsandv1.ListSnapshotsRequest{OrganizationId: org.ID}))
			if err != nil || len(list.Msg.GetSnapshots()) != 1 {
				return false
			}
			snapID = list.Msg.GetSnapshots()[0].GetSnapshotId()
			return true
		})
	testenv.Eventually(t, "snapshot FAILED", func() bool {
		return snapshotStateOf(t, c, org.ID, snapID) == ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED
	})

	agent.ResumeReads()
	sendReportApplied(t, env, agent)
	sendReportApplied(t, env, agent)
	got, err := getSnapshotErr(c, org.ID, snapID)
	if err != nil {
		t.Fatalf("GetSnapshot after the reports: %v", err)
	}
	if got.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED {
		t.Fatalf("state after the reports = %v, want FAILED", got.GetState())
	}
}

// TestStateReportOmittingReadySnapshot checks that the FAILED rule does not
// change the other omissions: an unexported READY snapshot that the report
// does not list is deleted, and an exported one keeps its record without a
// device.
func TestStateReportOmittingReadySnapshot(t *testing.T) {
	env := testenv.New(t)
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
	plainID := plainSnapshot(t, org, sandboxID)
	exportedID := exportedSnapshot(t, org)

	agent.ForgetSnapshot(plainID)
	agent.ForgetSnapshot(exportedID)
	sendReportApplied(t, env, agent)

	if _, err := getSnapshotErr(c, org.ID, plainID); testenv.Code(err) != connect.CodeNotFound {
		t.Fatalf("GetSnapshot of the omitted unexported snapshot: err = %v, want not_found", err)
	}
	got, err := getSnapshotErr(c, org.ID, exportedID)
	if err != nil {
		t.Fatalf("GetSnapshot of the omitted exported snapshot: %v", err)
	}
	if got.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY || got.GetExportState() != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED {
		t.Fatalf("exported snapshot = %v / %v, want READY / EXPORTED", got.GetState(), got.GetExportState())
	}
	var hasDevice bool
	if err := env.DB.Pool.QueryRow(ctx, "SELECT device_id IS NOT NULL FROM snapshots WHERE snapshot_id = $1", exportedID).Scan(&hasDevice); err != nil {
		t.Fatal(err)
	}
	if hasDevice {
		t.Fatal("the exported snapshot still names its device")
	}
}
