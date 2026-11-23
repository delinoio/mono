// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

var (
	macModelA = []byte("hardware-model-a")
	macModelB = []byte("hardware-model-b")
)

// macAgent enrolls and connects an Apple silicon Mac whose capability report
// lists the given macOS hardware models. Its snapshots carry macModelA.
func macAgent(t *testing.T, env *testenv.Env, org *testenv.Org, models ...[]byte) (*testenv.Device, *testenv.FakeAgent) {
	t.Helper()
	dev := env.EnrollDevice(org.ID, org.OwnerClient, ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64)
	if _, err := org.OwnerClient.Device.UpdateDeviceLimits(context.Background(), connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
		OrganizationId: org.ID, DeviceId: dev.ID,
		Limits: &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 65536, DiskGib: 400, VcpuOvercommitPercent: 100},
	})); err != nil {
		t.Fatal(err)
	}
	agent := env.NewFakeAgent(dev)
	agent.HostOS, agent.Arch = ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64
	agent.Capabilities = &ownsandv1.DeviceCapabilities{
		LogicalCpuCount: 8, MemoryMib: 65536, FreeDiskBytes: 500 << 30,
		GuestOses:            []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX, ownsandv1.GuestOs_GUEST_OS_MACOS},
		BuiltBaseImages:      []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_MACOS},
		MacosGuestsSupported: true,
		MacosHardwareModels:  models,
	}
	agent.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
		res := fa.DefaultAnswer(cmd)
		if snap := res.GetCreateSnapshot().GetSnapshot(); snap != nil {
			snap.GuestOs, snap.MacosHardwareModel, snap.HasMemoryState = ownsandv1.GuestOs_GUEST_OS_MACOS, macModelA, false
		}
		return res
	})
	agent.Connect()
	return dev, agent
}

func macCreateReq(orgID string) *ownsandv1.CreateSandboxRequest {
	req := createReq(orgID)
	req.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_MACOS}}
	req.Resources = &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 40}
	return req
}

// exportedMacSnapshot creates a macOS snapshot of macModelA on agent's
// device and exports it.
func exportedMacSnapshot(t *testing.T, org *testenv.Org, deviceID string) string {
	t.Helper()
	ctx := context.Background()
	c := org.OwnerClient
	req := macCreateReq(org.ID)
	req.Placement = &ownsandv1.Placement{DeviceId: &deviceID}
	created, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatal(err)
	}
	sandboxID := created.Msg.GetSandbox().GetSandboxId()
	waitState(t, c, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	snapID := plainSnapshot(t, org, sandboxID)
	// A running sandbox reserves the machine identifier of the snapshot, so
	// end the source sandbox before a create from the snapshot.
	deleteSandboxAndWait(t, org, sandboxID)
	if _, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID})); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "snapshot exported", func() bool {
		s, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && s.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	got, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
	if err != nil || string(got.Msg.GetSnapshot().GetMacosHardwareModel()) != string(macModelA) {
		t.Fatalf("snapshot hardware model = %q, %v", got.Msg.GetSnapshot().GetMacosHardwareModel(), err)
	}
	return snapID
}

// deleteSandboxAndWait deletes a sandbox and waits until the agent confirmed
// the deletion, which releases its machine identifier reservation.
func deleteSandboxAndWait(t *testing.T, org *testenv.Org, sandboxID string) {
	t.Helper()
	if _, err := org.OwnerClient.Sandbox.DeleteSandbox(context.Background(), connect.NewRequest(&ownsandv1.DeleteSandboxRequest{OrganizationId: org.ID, SandboxId: sandboxID})); err != nil {
		t.Fatal(err)
	}
	waitState(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
}

// macSnapshotCreate creates a sandbox from the snapshot, on deviceID when it
// is not empty.
func macSnapshotCreate(org *testenv.Org, snapID, deviceID string) (*ownsandv1.Sandbox, error) {
	req := macCreateReq(org.ID)
	req.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}}
	if deviceID != "" {
		req.Placement = &ownsandv1.Placement{DeviceId: &deviceID}
	}
	resp, err := org.OwnerClient.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(req))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetSandbox(), nil
}

// commandKinds lists the import and create commands that the agent received,
// in order.
func commandKinds(a *testenv.FakeAgent) []string {
	var out []string
	for _, cmd := range a.Commands() {
		switch {
		case cmd.GetImportSnapshot() != nil:
			out = append(out, "import")
		case cmd.GetCreateSandbox() != nil:
			out = append(out, "create")
		}
	}
	return out
}

func importModel(a *testenv.FakeAgent) []byte {
	for _, cmd := range a.Commands() {
		if imp := cmd.GetImportSnapshot(); imp != nil {
			return imp.GetSnapshot().GetMacosHardwareModel()
		}
	}
	return nil
}

// TestMacOSFirstImportWithUnreportedModel checks that a Mac whose capability
// report omits the hardware model of an exported macOS snapshot can import
// it, because it reports the model only after the import. The agent keeps
// the native check before the download.
func TestMacOSFirstImportWithUnreportedModel(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	devA, _ := macAgent(t, env, org, macModelA)
	snapID := exportedMacSnapshot(t, org, devA.ID)

	t.Run("explicit device imports then creates", func(t *testing.T) {
		devB, agentB := macAgent(t, env, org) // reports no model yet
		sb, err := macSnapshotCreate(org, snapID, devB.ID)
		if err != nil {
			t.Fatalf("create on a device that omits the model: %v", err)
		}
		waitState(t, c, org.ID, sb.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		defer deleteSandboxAndWait(t, org, sb.GetSandboxId())
		if got := commandKinds(agentB); len(got) != 2 || got[0] != "import" || got[1] != "create" {
			t.Fatalf("commands on the importing device = %v, want [import create]", got)
		}
		if got := importModel(agentB); !proto.Equal(&ownsandv1.SnapshotStateEntry{MacosHardwareModel: got}, &ownsandv1.SnapshotStateEntry{MacosHardwareModel: macModelA}) {
			t.Fatalf("import command model = %q, want %q", got, macModelA)
		}
	})

	t.Run("an unsupported model fails the import and sends no create", func(t *testing.T) {
		devB, agentB := macAgent(t, env, org, macModelB)
		agentB.SetOnCommand(func(fa *testenv.FakeAgent, cmd *ownsandv1.AgentCommand) *ownsandv1.AgentCommandResult {
			if cmd.GetImportSnapshot() != nil {
				return &ownsandv1.AgentCommandResult{Error: &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, Message: "the Mac does not support the hardware model"}}
			}
			return fa.DefaultAnswer(cmd)
		})
		sb, err := macSnapshotCreate(org, snapID, devB.ID)
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		failed := waitState(t, c, org.ID, sb.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_FAILED)
		if failed.GetEndReason() != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED {
			t.Fatalf("end reason = %v", failed.GetEndReason())
		}
		if got := commandKinds(agentB); len(got) != 1 || got[0] != "import" {
			t.Fatalf("commands after the rejected import = %v, want [import]", got)
		}
	})
}

// TestMacOSPlacementPrefersReportedModel checks that automatic placement
// uses a Mac that reports the hardware model before a less loaded Mac that
// does not report it, and uses the unreporting Mac when no other qualifies.
func TestMacOSPlacementPrefersReportedModel(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	c := org.OwnerClient
	devA, _ := macAgent(t, env, org, macModelA)
	snapID := exportedMacSnapshot(t, org, devA.ID)
	devUnknown, _ := macAgent(t, env, org)
	devReporting, _ := macAgent(t, env, org, macModelA)

	// Load the reporting Mac and the snapshot device, so the unknown Mac has
	// the lowest load of the three.
	for _, id := range []string{devReporting.ID, devA.ID} {
		req := macCreateReq(org.ID)
		req.Placement = &ownsandv1.Placement{DeviceId: &id}
		if _, err := c.Sandbox.CreateSandbox(context.Background(), connect.NewRequest(req)); err != nil {
			t.Fatalf("load %s: %v", id, err)
		}
	}
	sb, err := macSnapshotCreate(org, snapID, "")
	if err != nil {
		t.Fatalf("automatic create: %v", err)
	}
	if got := sb.GetDeviceId(); got == devUnknown.ID {
		t.Fatalf("automatic create ran on the Mac that does not report the model")
	}
	if sb.GetDeviceId() != devReporting.ID && sb.GetDeviceId() != devA.ID {
		t.Fatalf("automatic create ran on %q", sb.GetDeviceId())
	}
}

// TestMacOSPlacementUsesUnreportedModelWhenNoneQualifies checks that the Mac
// that does not report the hardware model runs the snapshot when no Mac that
// reports it can.
func TestMacOSPlacementUsesUnreportedModelWhenNoneQualifies(t *testing.T) {
	env := testenv.New(t)
	org := env.NewOrg("alice")
	devA, agentA := macAgent(t, env, org, macModelA)
	snapID := exportedMacSnapshot(t, org, devA.ID)
	devUnknown, _ := macAgent(t, env, org)
	agentA.Close() // the only device that reports the model goes offline
	testenv.Eventually(t, "snapshot device offline", func() bool { return env.App.Service.Hub.Live(devA.ID) == nil })

	sb, err := macSnapshotCreate(org, snapID, "")
	if err != nil {
		t.Fatalf("create with only an unreporting Mac online: %v", err)
	}
	if sb.GetDeviceId() != devUnknown.ID {
		t.Fatalf("create ran on %q, want the unreporting Mac %q", sb.GetDeviceId(), devUnknown.ID)
	}
}
