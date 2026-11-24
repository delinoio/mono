// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"math"
	"strings"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// TestHyperVMemoryRounding checks that a Windows (Hyper-V) device reserves an
// odd memory request rounded up to an even number of MiB, that admission and
// the reported allocation use that effective size, that the sandbox keeps the
// requested size, and that a Linux device keeps the requested size.
func TestHyperVMemoryRounding(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient

	enroll := func(hostOS ownsandv1.HostOs, memoryLimit int64) *testenv.Device {
		dev := env.EnrollDevice(org.ID, c, hostOS, ownsandv1.Arch_ARCH_AMD64)
		if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(&ownsandv1.UpdateDeviceLimitsRequest{
			OrganizationId: org.ID, DeviceId: dev.ID,
			Limits: &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: memoryLimit, DiskGib: 100, VcpuOvercommitPercent: 100},
		})); err != nil {
			t.Fatal(err)
		}
		agent := env.NewFakeAgent(dev)
		agent.HostOS = hostOS
		agent.Connect()
		return dev
	}
	create := func(deviceID string, memoryMiB int64) (*ownsandv1.Sandbox, error) {
		req := createReq(org.ID)
		req.Placement = &ownsandv1.Placement{DeviceId: &deviceID}
		req.Resources.MemoryMib = memoryMiB
		resp, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req))
		if err != nil {
			return nil, err
		}
		return resp.Msg.GetSandbox(), nil
	}
	allocated := func(deviceID string) int64 {
		t.Helper()
		got, err := c.Device.GetDevice(ctx, connect.NewRequest(&ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: deviceID}))
		if err != nil {
			t.Fatal(err)
		}
		list, err := c.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID}))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range list.Msg.GetDevices() {
			if d.GetDeviceId() == deviceID && d.GetAllocation().GetMemoryMib() != got.Msg.GetDevice().GetAllocation().GetMemoryMib() {
				t.Fatalf("get and list disagree: %v and %v", got.Msg.GetDevice().GetAllocation(), d.GetAllocation())
			}
		}
		return got.Msg.GetDevice().GetAllocation().GetMemoryMib()
	}

	// An odd request at an odd cap would reserve one MiB above the cap.
	win := enroll(ownsandv1.HostOs_HOST_OS_WINDOWS, 1023)
	if _, err := create(win.ID, 1023); testenv.Code(err) != connect.CodeFailedPrecondition {
		t.Fatalf("odd request at an odd cap: %v", err)
	}
	if got := allocated(win.ID); got != 0 {
		t.Fatalf("allocation after the rejected request = %d", got)
	}

	// Odd requests reserve the rounded size; the sandbox keeps its request.
	win = enroll(ownsandv1.HostOs_HOST_OS_WINDOWS, 2048)
	sb, err := create(win.ID, 1023)
	if err != nil {
		t.Fatalf("odd request with room: %v", err)
	}
	if got := sb.GetResources().GetMemoryMib(); got != 1023 {
		t.Fatalf("sandbox memory = %d, want the requested 1023", got)
	}
	if got := allocated(win.ID); got != 1024 {
		t.Fatalf("allocation after one odd request = %d, want 1024", got)
	}
	if _, err := create(win.ID, 1023); err != nil {
		t.Fatalf("second odd request with room: %v", err)
	}
	if got := allocated(win.ID); got != 2048 {
		t.Fatalf("allocation after two odd requests = %d, want 2048", got)
	}
	// The rounded reservations fill the cap: even 2 MiB do not fit, while the
	// unrounded sum 2046 would leave room for them.
	if _, err := create(win.ID, 2); testenv.Code(err) != connect.CodeUnavailable {
		t.Fatalf("request above the rounded allocation: %v", err)
	}

	// A Linux device reserves the requested size.
	lin := enroll(ownsandv1.HostOs_HOST_OS_LINUX, 1023)
	if _, err := create(lin.ID, 1023); err != nil {
		t.Fatalf("odd request on a Linux device: %v", err)
	}
	if got := allocated(lin.ID); got != 1023 {
		t.Fatalf("Linux allocation = %d, want 1023", got)
	}
}

func TestHyperVMemoryMaxInt64(t *testing.T) {
	env := testenv.New(t)
	ctx := context.Background()
	org := env.NewOrg("alice")
	c := org.OwnerClient

	for _, tc := range []struct {
		name      string
		hostOS    ownsandv1.HostOs
		requested int64
		want      int64
	}{
		{"Windows at MaxInt64", ownsandv1.HostOs_HOST_OS_WINDOWS, math.MaxInt64, math.MaxInt64},
		{"Windows odd below MaxInt64", ownsandv1.HostOs_HOST_OS_WINDOWS, math.MaxInt64 - 2, math.MaxInt64 - 1},
		{"Windows even below MaxInt64", ownsandv1.HostOs_HOST_OS_WINDOWS, math.MaxInt64 - 1, math.MaxInt64 - 1},
		{"Linux odd below MaxInt64", ownsandv1.HostOs_HOST_OS_LINUX, math.MaxInt64 - 2, math.MaxInt64 - 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dev := env.EnrollDevice(org.ID, c, tc.hostOS, ownsandv1.Arch_ARCH_AMD64)
			limits := &ownsandv1.UpdateDeviceLimitsRequest{
				OrganizationId: org.ID, DeviceId: dev.ID,
				Limits: &ownsandv1.DeviceLimits{VcpuCount: 64, MemoryMib: math.MaxInt64, DiskGib: 1000, VcpuOvercommitPercent: 100},
			}
			if _, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(limits)); err != nil {
				t.Fatalf("set initial device limits: %v", err)
			}
			agent := env.NewFakeAgent(dev)
			agent.HostOS = tc.hostOS
			agent.Connect()

			// Each case uses a separate device in the same organization, so later
			// creates also read the saturated reservation during placement.
			req := createReq(org.ID)
			req.Placement = &ownsandv1.Placement{DeviceId: &dev.ID}
			req.Resources.MemoryMib = tc.requested
			if _, err := c.Sandbox.CreateSandbox(ctx, connect.NewRequest(req)); err != nil {
				t.Fatalf("CreateSandbox: %v", err)
			}

			getReq := &ownsandv1.GetDeviceRequest{OrganizationId: org.ID, DeviceId: dev.ID}
			got, err := c.Device.GetDevice(ctx, connect.NewRequest(getReq))
			if err != nil {
				t.Fatalf("GetDevice: %v", err)
			}
			if memory := got.Msg.GetDevice().GetAllocation().GetMemoryMib(); memory != tc.want {
				t.Errorf("GetDevice allocated memory = %d, want %d", memory, tc.want)
			}

			list, err := c.Device.ListDevices(ctx, connect.NewRequest(&ownsandv1.ListDevicesRequest{OrganizationId: org.ID}))
			if err != nil {
				t.Fatalf("ListDevices: %v", err)
			}
			var listed *ownsandv1.Device
			for _, d := range list.Msg.GetDevices() {
				if d.GetDeviceId() == dev.ID {
					listed = d
					break
				}
			}
			if listed == nil {
				t.Fatal("ListDevices did not return the device")
			}
			if memory := listed.GetAllocation().GetMemoryMib(); memory != tc.want {
				t.Errorf("ListDevices allocated memory = %d, want %d", memory, tc.want)
			}

			if tc.requested == math.MaxInt64 {
				limits.Limits.MemoryMib = 8192
				updated, err := c.Device.UpdateDeviceLimits(ctx, connect.NewRequest(limits))
				if err != nil {
					t.Fatalf("lower device memory limit: %v", err)
				}
				if memory := updated.Msg.GetDevice().GetAllocation().GetMemoryMib(); memory != tc.want {
					t.Errorf("UpdateDeviceLimits allocated memory = %d, want %d", memory, tc.want)
				}
				got, err := c.Device.GetDevice(ctx, connect.NewRequest(getReq))
				if err != nil {
					t.Fatalf("GetDevice after lowering memory limit: %v", err)
				}
				if memory := got.Msg.GetDevice().GetLimits().GetMemoryMib(); memory != limits.Limits.MemoryMib {
					t.Errorf("stored memory limit = %d, want %d", memory, limits.Limits.MemoryMib)
				}
			}
		})
	}

	if strings.Contains(env.Logs.String(), "SQLSTATE 22003") {
		t.Error("the control plane logged a PostgreSQL numeric overflow")
	}
}
