// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// removeDeviceRPCForCopyTest removes the device through the RemoveDevice RPC.
func removeDeviceRPCForCopyTest(t *testing.T, org *testenv.Org, deviceID string) {
	t.Helper()
	if _, err := org.OwnerClient.Device.RemoveDevice(context.Background(), connect.NewRequest(&ownsandv1.RemoveDeviceRequest{OrganizationId: org.ID, DeviceId: deviceID})); err != nil {
		t.Fatal(err)
	}
}

// TestImportResultWaitsForInFlightDeviceRemoval checks the lock order: a
// result that arrives while a removal holds the device row FOR UPDATE and has
// already deleted the copy rows of the device waits for it and, when the
// removal commits, registers no copy row for the removed device and sends no
// delete_snapshot command.
func TestImportResultWaitsForInFlightDeviceRemoval(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)

	// A removal that has locked the device, marked it removed and deleted its
	// copy rows (the order of removeDeviceTx) but has not committed yet.
	tx, err := env.DB.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE", devB.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "UPDATE devices SET state = $2 WHERE device_id = $1", devB.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM snapshot_copies WHERE device_id = $1", devB.ID); err != nil {
		t.Fatal(err)
	}

	sendImportResult(agentB, cmdID)
	// The result must be blocked on the device row, not applied: the row it
	// would insert is checked after the removal commits.
	time.Sleep(500 * time.Millisecond)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "removed-device result logged", func() bool {
		return strings.Contains(env.Logs.String(), `"snapshot_import_result_device_removed"`)
	})
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after the removal committed = %d, want 0", n)
	}
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("delete commands for the removed device = %d, want 0", n)
	}
	if strings.Contains(env.Logs.String(), "agent_command_result_apply_failed") {
		t.Fatalf("the result was logged as an apply failure:\n%s", env.Logs.String())
	}
}

// TestImportResultAfterDeviceRemovalRegistersNoCopy checks the other order:
// a result that is handled after the removal committed, while its channel is
// still open (the removal closes the channel after its transaction), writes no
// copy row. The removal runs through the RPC; the result that the agent sends
// after it may reach the server before or after the channel closes, and
// neither order may leave a row.
func TestImportResultAfterDeviceRemovalRegistersNoCopy(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)

	removeDeviceRPCForCopyTest(t, org, devB.ID)
	sendImportResult(agentB, cmdID)
	time.Sleep(500 * time.Millisecond)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after the removal = %d, want 0", n)
	}
	if n := snapshotRowCount(t, env, snapID); n != 1 {
		t.Fatalf("snapshot rows = %d, want 1: an exported snapshot survives the removal of a device without its local record", n)
	}
}

// TestImportResultOnOpenChannelAfterRemovalCommitRegistersNoCopy checks that a result
// that the server handles on a still-open channel after the removal committed
// registers no row. The removal is committed directly in the database in the
// order of removeDeviceTx, so the channel stays open and the result is
// handled; it is the window between the commit and the channel closure.
func TestImportResultOnOpenChannelAfterRemovalCommitRegistersNoCopy(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	snapID, devB, agentB := importFixture(t, env, org)
	importID := withholdImportResult(agentB)
	cmdID := startWithheldImport(t, org, snapID, devB.ID, agentB, importID)

	if _, err := env.DB.Pool.Exec(ctx, "UPDATE devices SET state = $2 WHERE device_id = $1", devB.ID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)); err != nil {
		t.Fatal(err)
	}
	sendImportResult(agentB, cmdID)
	testenv.Eventually(t, "removed-device result logged", func() bool {
		return strings.Contains(env.Logs.String(), `"snapshot_import_result_device_removed"`)
	})
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows for a removed device = %d, want 0", n)
	}
	if n := deleteSnapshotCommands(agentB, snapID); n != 0 {
		t.Fatalf("delete commands for the removed device = %d, want 0", n)
	}
}

// TestCopyRowRegisteredBeforeDeviceRemovalIsCleared checks that a copy row
// that an import registered before the removal is deleted by the removal.
func TestCopyRowRegisteredBeforeDeviceRemovalIsCleared(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	snapID, devB, _ := importFixture(t, env, org)

	created, err := c.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(importCreateReq(org.ID, snapID, devB.ID)))
	if err != nil {
		t.Fatal(err)
	}
	waitState(t, c, org.ID, created.Msg.GetSandbox().GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 1 {
		t.Fatalf("copy rows after the import = %d, want 1 (an import on an active device registers its copy)", n)
	}
	removeDeviceRPCForCopyTest(t, org, devB.ID)
	if n := copyRowCount(t, env, snapID, devB.ID); n != 0 {
		t.Fatalf("copy rows after the removal = %d, want 0", n)
	}
}
