// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/scheduler"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestOutboundNetworkCheckPlacement checks that the control plane skips a
// device that reports a failed outbound-network check for an OUTBOUND
// sandbox even when it has the lower load, that pinning that device fails
// with failed_precondition and sends no create command, and that a NONE
// sandbox still runs there.
func TestOutboundNetworkCheckPlacement(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient

	enroll := func(outboundPassed bool) (*testenv.Device, *testenv.FakeAgent) {
		dev := env.EnrollDevice(org.ID, c, ownsandv1.HostOs_HOST_OS_LINUX, ownsandv1.Arch_ARCH_AMD64)
		if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
			OrganizationId: org.ID, DeviceId: dev.ID,
			Limits: &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 16384, DiskGib: 100, VcpuOvercommitPercent: 100},
		})); err != nil {
			t.Fatal(err)
		}
		agent := env.NewFakeAgent(dev)
		agent.Capabilities.HypervisorChecks = []*ownsandv1.HypervisorCheck{
			{CheckId: scheduler.CheckOutboundNetwork, Passed: outboundPassed, Detail: "fixture"},
		}
		agent.Connect()
		return dev, agent
	}
	failedDev, failedAgent := enroll(false)
	passedDev, _ := enroll(true)
	create := func(mutate func(*ownsandv1.CreateSandboxRequest)) (*ownsandv1.Sandbox, error) {
		req := createReq(org.ID)
		mutate(req)
		resp, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetSandbox(), nil
	}
	createCommands := func(a *testenv.FakeAgent) int {
		n := 0
		for _, cmd := range a.Commands() {
			if cmd.GetCreateSandbox() != nil {
				n++
			}
		}
		return n
	}
	outbound := func(r *ownsandv1.CreateSandboxRequest) {
		r.Network = &ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND}
	}

	// Load the passed device first, so the failed device has the lower load
	// and automatic placement would pick it without the rule.
	pin := func(id string) func(*ownsandv1.CreateSandboxRequest) {
		return func(r *ownsandv1.CreateSandboxRequest) {
			outbound(r)
			r.Placement = &ownsandv1.Placement{DeviceId: &id}
		}
	}
	if _, err := create(pin(passedDev.ID)); err != nil {
		t.Fatalf("pinned create on the passed device: %v", err)
	}
	sb, err := create(outbound)
	if err != nil {
		t.Fatalf("automatic OUTBOUND create: %v", err)
	}
	if sb.GetDeviceId() != passedDev.ID {
		t.Fatalf("automatic OUTBOUND create ran on %q, want the passed device %q", sb.GetDeviceId(), passedDev.ID)
	}

	// Pinning the failed device is a permanent mismatch and sends no command.
	if _, err := create(pin(failedDev.ID)); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("pinned OUTBOUND create on the failed device: %v", err)
	}
	if n := createCommands(failedAgent); n != 0 {
		t.Fatalf("the failed device received %d create commands", n)
	}

	// A NONE sandbox does not need outbound networking.
	if sb, err = create(func(r *ownsandv1.CreateSandboxRequest) {
		r.Network = &ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_NONE}
		r.Placement = &ownsandv1.Placement{DeviceId: &failedDev.ID}
	}); err != nil {
		t.Fatalf("pinned NONE create on the failed device: %v", err)
	}
	if sb.GetDeviceId() != failedDev.ID {
		t.Fatalf("NONE create ran on %q, want %q", sb.GetDeviceId(), failedDev.ID)
	}
}

func TestWindowsGuestOutboundPlacement(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient
	enroll := func() *testenv.FakeAgent {
		dev := env.EnrollDevice(org.ID, c, ownsandv1.HostOs_HOST_OS_WINDOWS, ownsandv1.Arch_ARCH_AMD64)
		if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
			OrganizationId: org.ID, DeviceId: dev.ID,
			Limits: &ownsandv1.DeviceLimits{VcpuCount: 32, MemoryMib: 65536, DiskGib: 600, VcpuOvercommitPercent: 100},
		})); err != nil {
			t.Fatal(err)
		}
		agent := env.NewFakeAgent(dev)
		agent.HostOS = ownsandv1.HostOs_HOST_OS_WINDOWS
		agent.Capabilities.FreeDiskBytes = 600 << 30
		agent.Capabilities.GuestOses = append(agent.Capabilities.GuestOses, ownsandv1.GuestOs_GUEST_OS_WINDOWS)
		agent.Capabilities.BuiltBaseImages = append(agent.Capabilities.BuiltBaseImages, ownsandv1.BaseImage_BASE_IMAGE_WINDOWS)
		agent.Capabilities.HypervisorChecks = []*ownsandv1.HypervisorCheck{{CheckId: scheduler.CheckOutboundNetwork, Passed: true}}
		agent.Connect()
		return agent
	}
	origin, target := enroll(), enroll()
	windowsImage := &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: ownsandv1.BaseImage_BASE_IMAGE_WINDOWS}}

	createControl := func(req *ownsandv1.CreateSandboxRequest, agent *testenv.FakeAgent) string {
		t.Helper()
		req.Placement = &ownsandv1.Placement{DeviceId: &agent.Device.ID}
		resp, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
		if err != nil {
			t.Fatalf("create control sandbox: %v", err)
		}
		sb := resp.Msg.GetSandbox()
		if sb.GetDeviceId() != agent.Device.ID {
			t.Fatalf("control device = %s, want %s", sb.GetDeviceId(), agent.Device.ID)
		}
		waitState(t, c, org.ID, sb.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		return sb.GetSandboxId()
	}
	windowsNone := createReq(org.ID)
	windowsNone.Source = windowsImage
	windowsNone.Resources = &ownsandv1.Resources{VcpuCount: 2, MemoryMib: 4096, DiskGib: 40}
	windowsNone.Network = &ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_NONE}
	snapID := plainSnapshot(t, org, createControl(windowsNone, origin))
	if _, err := c.Snapshot.ExportSnapshot(ctx, connect.NewRequest(&ownsandv1.ExportSnapshotRequest{
		OrganizationId: org.ID, SnapshotId: snapID,
	})); err != nil {
		t.Fatal(err)
	}
	testenv.Eventually(t, "Windows snapshot exported", func() bool {
		resp, err := c.Snapshot.GetSnapshot(ctx, connect.NewRequest(&ownsandv1.GetSnapshotRequest{OrganizationId: org.ID, SnapshotId: snapID}))
		return err == nil && resp.Msg.GetSnapshot().GetGuestOs() == ownsandv1.GuestOs_GUEST_OS_WINDOWS &&
			resp.Msg.GetSnapshot().GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED
	})
	tmpl, err := c.Snapshot.CreateTemplate(ctx, connect.NewRequest(&ownsandv1.CreateTemplateRequest{
		OrganizationId: org.ID, Name: "windows-base", SnapshotId: snapID,
	}))
	if err != nil {
		t.Fatal(err)
	}
	if target.HasSnapshot(snapID) {
		t.Fatal("the target already holds the exported snapshot")
	}
	linuxOutbound := createReq(org.ID)
	linuxOutbound.Network = &ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND}
	createControl(linuxOutbound, target)

	sources := []struct {
		name   string
		source *ownsandv1.SandboxSource
	}{
		{"base_image", windowsImage},
		{"snapshot", &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: snapID}}},
		{"template", &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: tmpl.Msg.GetTemplate().GetTemplateId()}}},
	}
	modes := []struct {
		name    string
		network *ownsandv1.Network
	}{
		{"outbound", &ownsandv1.Network{Mode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND}},
		{"omitted", nil},
	}
	placements := []struct {
		name     string
		deviceID *string
	}{
		{"automatic", nil},
		{"explicit", &target.Device.ID},
	}
	for _, state := range []string{"online", "offline"} {
		if state == "offline" {
			for _, agent := range []*testenv.FakeAgent{origin, target} {
				agent.Close()
				testenv.Eventually(t, "device offline", func() bool {
					resp, err := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{
						OrganizationId: org.ID, DeviceId: agent.Device.ID,
					}))
					return err == nil && resp.Msg.GetDevice().GetState() == ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
				})
			}
		}
		for _, source := range sources {
			for _, mode := range modes {
				for _, placement := range placements {
					t.Run(state+"/"+source.name+"/"+mode.name+"/"+placement.name, func(t *testing.T) {
						req := createReq(org.ID)
						req.Source, req.Network = source.source, mode.network
						req.Resources = windowsNone.Resources
						req.Placement = &ownsandv1.Placement{DeviceId: placement.deviceID}
						_, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
						if testenv.Code(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "Windows guests require NONE networking") {
							t.Errorf("CreateSandbox = %v, want failed_precondition with the Windows network mode", err)
						}
					})
				}
			}
		}
	}

	unknownDevice := createReq(org.ID)
	unknownID := unknownDevice.IdempotencyKey
	unknownDevice.Source = windowsImage
	unknownDevice.Placement = &ownsandv1.Placement{DeviceId: &unknownID}
	if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(unknownDevice)); testenv.Code(err) != connect.CodeNotFound {
		t.Errorf("CreateSandbox on an unknown device = %v, want not_found", err)
	}

	// Only the controls reserve capacity and store create keys.
	var sandboxes, keys int
	if err := env.DB.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM sandboxes WHERE organization_id = $1),
		(SELECT count(*) FROM sandbox_create_keys WHERE organization_id = $1)`, org.ID).Scan(&sandboxes, &keys); err != nil {
		t.Fatal(err)
	}
	if sandboxes != 2 || keys != 2 {
		t.Errorf("found %d sandboxes and %d create keys, want two of each for the controls", sandboxes, keys)
	}
	for _, agent := range []*testenv.FakeAgent{origin, target} {
		creates, imports := 0, 0
		for _, cmd := range agent.Commands() {
			if cmd.GetCreateSandbox() != nil {
				creates++
			}
			if cmd.GetImportSnapshot() != nil {
				imports++
			}
		}
		if creates != 1 || imports != 0 {
			t.Errorf("device %s received %d creates and %d imports, want one create and zero imports", agent.Device.ID, creates, imports)
		}
	}
}
