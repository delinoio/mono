// SPDX-License-Identifier: FSL-1.1-ALv2

package scheduler

import (
	"errors"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var now = time.Unix(1_800_000_000, 0)

func linux(id string) Device {
	return Device{
		ID: id, State: ownsandv1.DeviceState_DEVICE_STATE_ONLINE, LiveChannel: true,
		Caps: &ownsandv1.DeviceCapabilities{
			HostOs: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, FreeDiskBytes: 1 << 40,
			GuestOses: []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX}, BuiltBaseImages: []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX},
		},
		Limits:      Resources{8, 16384, 100},
		UpdateState: ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE,
	}
}

func mac(id string) Device {
	d := linux(id)
	d.Caps.HostOs, d.Caps.Arch = ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64
	d.Caps.GuestOses = []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX, ownsandv1.GuestOs_GUEST_OS_MACOS}
	d.Caps.BuiltBaseImages = []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_MACOS}
	d.Caps.MacosGuestsSupported = true
	d.Caps.MacosHardwareModels = []string{"hw1"}
	return d
}

func linuxReq() Request {
	return Request{GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX, Resources: Resources{2, 2048, 10}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND}
}

func code(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, ErrUnavailable):
		return "unavailable"
	case errors.Is(err, ErrFailedPrecondition):
		return "failed_precondition"
	}
	return err.Error()
}

func TestEligibilityAndErrorCodes(t *testing.T) {
	hostMac := ownsandv1.HostOs_HOST_OS_MACOS
	long := now.Add(-25 * time.Hour)
	short := now.Add(-time.Hour)
	cases := []struct {
		name string
		mut  func(*Device, *Request)
		want string
	}{
		{"eligible", func(*Device, *Request) {}, "ok"},
		{"offline", func(d *Device, _ *Request) { d.State = ownsandv1.DeviceState_DEVICE_STATE_OFFLINE }, "unavailable"},
		{"no live channel", func(d *Device, _ *Request) { d.LiveChannel = false }, "unavailable"},
		{"removed", func(d *Device, _ *Request) { d.State = ownsandv1.DeviceState_DEVICE_STATE_REMOVED }, "failed_precondition"},
		{"new device with zero limits", func(d *Device, _ *Request) { d.Limits = Resources{} }, "failed_precondition"},
		{"over the limits themselves", func(_ *Device, r *Request) { r.Resources.VCPUCount = 9 }, "failed_precondition"},
		{"limits below allocation", func(d *Device, _ *Request) { d.Limits.VCPUCount = 2; d.Allocated.VCPUCount = 4 }, "unavailable"},
		{"capacity held", func(d *Device, _ *Request) { d.Allocated = Resources{7, 0, 0} }, "unavailable"},
		{"deleting sandboxes hold capacity", func(d *Device, _ *Request) { d.Allocated = Resources{0, 15000, 0} }, "unavailable"},
		{"free disk", func(d *Device, _ *Request) { d.Caps.FreeDiskBytes = 5 << 30 }, "unavailable"},
		{"unreported creating disk", func(d *Device, _ *Request) { d.Caps.FreeDiskBytes = 15 << 30; d.UnreportedCreatingDiskGiB = 10 }, "unavailable"},
		{"host OS filter", func(_ *Device, r *Request) { r.HostOS = &hostMac }, "failed_precondition"},
		{"guest unsupported", func(_ *Device, r *Request) { r.GuestOS = ownsandv1.GuestOs_GUEST_OS_WINDOWS }, "failed_precondition"},
		{"base image not built", func(_ *Device, r *Request) { r.BaseImage = ownsandv1.BaseImage_BASE_IMAGE_WINDOWS }, "failed_precondition"},
		{"installing", func(d *Device, _ *Request) { d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING }, "unavailable"},
		{"revoked version", func(d *Device, _ *Request) { d.VersionRevoked = true }, "unavailable"},
		{"revoked pending idle", func(d *Device, _ *Request) {
			d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE
		}, "unavailable"},
		{"pending idle 1h", func(d *Device, _ *Request) {
			d.UpdateState, d.PendingIdleSince = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE, &short
		}, "ok"},
		{"pending idle 25h", func(d *Device, _ *Request) {
			d.UpdateState, d.PendingIdleSince = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE, &long
		}, "unavailable"},
		{"permanent beats transient", func(d *Device, r *Request) {
			d.State = ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
			r.Resources.VCPUCount = 64
		}, "failed_precondition"},
	}
	for _, tc := range cases {
		d, r := linux("d1"), linuxReq()
		tc.mut(&d, &r)
		_, err := Place([]Device{d}, r, now)
		if got := code(err); got != tc.want {
			t.Errorf("%s: got %s, want %s", tc.name, got, tc.want)
		}
		r.DeviceID = "d1"
		if _, err := Place([]Device{d, linux("d0")}, r, now); code(err) != tc.want {
			t.Errorf("%s with device_id: got %s, want %s", tc.name, code(err), tc.want)
		}
	}
}

func TestMacOSRules(t *testing.T) {
	req := Request{GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, BaseImage: ownsandv1.BaseImage_BASE_IMAGE_MACOS, Resources: Resources{2, 2048, 10}}
	d := mac("m1")
	if _, err := Place([]Device{d}, req, now); err != nil {
		t.Fatal(err)
	}
	d.MacOSGuests = 2
	if _, err := Place([]Device{d}, req, now); code(err) != "unavailable" {
		t.Fatalf("third macOS guest: %v", err)
	}
	// Linux guests do not count toward the macOS limit.
	if _, err := Place([]Device{d}, Request{GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, BaseImage: ownsandv1.BaseImage_BASE_IMAGE_MACOS, Resources: req.Resources}, now); err != nil {
		t.Fatalf("linux guest on a busy Mac: %v", err)
	}
	intel := mac("m2")
	intel.Caps.Arch = ownsandv1.Arch_ARCH_AMD64
	if _, err := Place([]Device{intel}, req, now); code(err) != "failed_precondition" {
		t.Fatalf("intel Mac: %v", err)
	}
	d = mac("m1")
	d.MachineIdentifierReserved = true
	if _, err := Place([]Device{d}, req, now); code(err) != "unavailable" {
		t.Fatalf("reserved identifier: %v", err)
	}
	snap := &Snapshot{ID: "s", DeviceID: "m1", GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, MacOSHardwareModel: "hw2"}
	if _, err := Place([]Device{mac("m1")}, Request{GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, Snapshot: snap, Resources: req.Resources}, now); code(err) != "failed_precondition" {
		t.Fatalf("unsupported hardware model: %v", err)
	}
}

func TestSnapshotPlacementAndSelection(t *testing.T) {
	snap := &Snapshot{
		ID: "s", DeviceID: "b", Exported: true, HostOS: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64,
		GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, HasMemoryState: true, Resources: Resources{2, 2048, 10}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND,
	}
	req := Request{GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, Snapshot: snap, Resources: Resources{2, 2048, 10}, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND}
	a, b := linux("a"), linux("b")
	b.Allocated = Resources{6, 0, 0} // busier, but memory state restores there
	p, err := Place([]Device{a, b}, req, now)
	if err != nil || p.DeviceID != "b" || !p.MemoryRestore {
		t.Fatalf("memory restore preference: %+v %v", p, err)
	}
	req.Resources.MemoryMiB = 4096 // memory state cannot restore: least loaded wins
	p, err = Place([]Device{a, b}, req, now)
	if err != nil || p.DeviceID != "a" || p.MemoryRestore {
		t.Fatalf("cold boot: %+v %v", p, err)
	}
	snap.Exported = false
	p, err = Place([]Device{a, b}, req, now)
	if err != nil || p.DeviceID != "b" {
		t.Fatalf("local-only snapshot: %+v %v", p, err)
	}
	arm := linux("c")
	arm.Caps.Arch = ownsandv1.Arch_ARCH_ARM64
	snap.Exported = true
	snap.DeviceID = ""
	if _, err := Place([]Device{arm}, req, now); code(err) != "failed_precondition" {
		t.Fatalf("exported snapshot on another architecture: %v", err)
	}
	// Ties break by the lowest device ID.
	x, y := linux("y"), linux("x")
	p, _ = Place([]Device{x, y}, linuxReq(), now)
	if p.DeviceID != "x" {
		t.Fatalf("tie: %s", p.DeviceID)
	}
	// Without device_id, one transiently ineligible device makes the error unavailable.
	off := linux("o")
	off.State = ownsandv1.DeviceState_DEVICE_STATE_OFFLINE
	small := linux("s")
	small.Limits = Resources{1, 1, 1}
	if _, err := Place([]Device{small, off}, linuxReq(), now); code(err) != "unavailable" {
		t.Fatalf("mixed: %v", err)
	}
	if _, err := Place([]Device{small}, linuxReq(), now); code(err) != "failed_precondition" {
		t.Fatalf("permanent only: %v", err)
	}
	if _, err := Place(nil, linuxReq(), now); code(err) != "failed_precondition" {
		t.Fatalf("no devices: %v", err)
	}
}
