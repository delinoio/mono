// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
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
