// SPDX-License-Identifier: FSL-1.1-ALv2

package scheduler

import (
	"errors"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var now = time.Unix(1_800_000_000, 0)

func linuxDevice(id string) Device {
	return Device{
		ID: id, Online: true, LiveChannel: true,
		HostOS: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64,
		Capabilities: &ownsandv1.DeviceCapabilities{
			GuestOses:       []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX},
			BuiltBaseImages: []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_LINUX},
			FreeDiskBytes:   100 << 30,
		},
		Limits:     &ownsandv1.DeviceLimits{VcpuCount: 4, MemoryMib: 4096, DiskGib: 50, VcpuOvercommitPercent: 100},
		Allocation: &ownsandv1.DeviceAllocation{},
	}
}

func macDevice(id string) Device {
	d := linuxDevice(id)
	d.HostOS, d.Arch = ownsandv1.HostOs_HOST_OS_MACOS, ownsandv1.Arch_ARCH_ARM64
	d.Capabilities = &ownsandv1.DeviceCapabilities{
		GuestOses:            []ownsandv1.GuestOs{ownsandv1.GuestOs_GUEST_OS_LINUX, ownsandv1.GuestOs_GUEST_OS_MACOS},
		BuiltBaseImages:      []ownsandv1.BaseImage{ownsandv1.BaseImage_BASE_IMAGE_MACOS},
		MacosGuestsSupported: true,
		FreeDiskBytes:        500 << 30,
		MacosHardwareModels:  [][]byte{[]byte("model-a")},
	}
	d.Limits = &ownsandv1.DeviceLimits{VcpuCount: 16, MemoryMib: 65536, DiskGib: 400, VcpuOvercommitPercent: 100}
	return d
}

func linuxRequest() Request {
	return Request{VCPUCount: 2, MemoryMiB: 1024, DiskGiB: 10, GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX, BaseImage: ownsandv1.BaseImage_BASE_IMAGE_LINUX, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND}
}

func code(err error) ownsandv1.ErrorCode {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return 0
}

func TestPlaceSelectsLowestShareThenID(t *testing.T) {
	a, b, c := linuxDevice("b-device"), linuxDevice("a-device"), linuxDevice("c-device")
	a.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 1}
	c.Allocation = &ownsandv1.DeviceAllocation{MemoryMib: 3000}
	dec, err := Place(linuxRequest(), []Device{a, b, c}, now)
	if err != nil || dec.DeviceID != "a-device" {
		t.Fatalf("decision = %+v, %v", dec, err)
	}
	b.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 2}
	dec, _ = Place(linuxRequest(), []Device{a, b, c}, now)
	if dec.DeviceID != "b-device" { // a: 1/4, b: 2/4, c: 3000/4096
		t.Fatalf("decision = %+v", dec)
	}
}

func TestErrorCodeRule(t *testing.T) {
	offline := linuxDevice("d1")
	offline.Online = false
	if _, err := Place(linuxRequest(), []Device{offline}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("offline: %v", err)
	}
	noChannel := linuxDevice("d1")
	noChannel.LiveChannel = false
	if _, err := Place(linuxRequest(), []Device{noChannel}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("no live channel: %v", err)
	}
	full := linuxDevice("d1")
	full.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 3}
	if _, err := Place(linuxRequest(), []Device{full}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("capacity: %v", err)
	}
	big := linuxRequest()
	big.VCPUCount = 5
	if _, err := Place(big, []Device{linuxDevice("d1")}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("over the limit: %v", err)
	}
	// Without device_id: one transient device makes the answer unavailable.
	if _, err := Place(big, []Device{linuxDevice("d1"), full}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("big request everywhere: %v", err)
	}
	if _, err := Place(linuxRequest(), []Device{macDevice("m1"), full}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("one transient device: %v", err)
	}
	// With device_id, only that device counts.
	req := linuxRequest()
	req.DeviceID = "m1"
	if _, err := Place(req, []Device{macDevice("m1"), linuxDevice("d1")}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("explicit device without the base image: %v", err)
	}
	req.DeviceID = "missing"
	if _, err := Place(req, []Device{linuxDevice("d1")}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("explicit unknown device: %v", err)
	}
	filter := linuxRequest()
	filter.Arch = ownsandv1.Arch_ARCH_ARM64
	if _, err := Place(filter, []Device{linuxDevice("d1")}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("arch filter: %v", err)
	}
}

func TestVCPUOvercommit(t *testing.T) {
	d := linuxDevice("d1")
	d.Limits.VcpuOvercommitPercent = 200 // effective 8
	d.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 6}
	if _, err := Place(linuxRequest(), []Device{d}, now); err != nil {
		t.Fatalf("overcommitted capacity: %v", err)
	}
	d.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 7}
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("beyond the effective limit: %v", err)
	}
	// One sandbox never gets more vCPUs than the vCPU limit itself.
	d.Allocation = &ownsandv1.DeviceAllocation{}
	req := linuxRequest()
	req.VCPUCount = 5
	if _, err := Place(req, []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("one sandbox over the vCPU limit: %v", err)
	}
	// Memory is never overcommitted.
	d.Allocation = &ownsandv1.DeviceAllocation{MemoryMib: 3500}
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("memory: %v", err)
	}
}

func TestMacOSRules(t *testing.T) {
	req := Request{VCPUCount: 4, MemoryMiB: 8192, DiskGiB: 80, GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, BaseImage: ownsandv1.BaseImage_BASE_IMAGE_MACOS}
	m := macDevice("m1")
	if _, err := Place(req, []Device{m}, now); err != nil {
		t.Fatalf("mac: %v", err)
	}
	// Two macOS guests, also when one is DELETING, block a third.
	m.Allocation = &ownsandv1.DeviceAllocation{MacosGuestCount: 2, VcpuCount: 8, MemoryMib: 16384, DiskGib: 160}
	if _, err := Place(req, []Device{m}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("third macOS guest: %v", err)
	}
	// Linux guests on the Mac do not count toward the limit.
	linux := linuxRequest()
	linux.BaseImage = 0
	if _, err := Place(linux, []Device{m}, now); err != nil {
		t.Fatalf("linux guest on a full Mac: %v", err)
	}
	if _, err := Place(req, []Device{linuxDevice("d1")}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("macOS on Linux: %v", err)
	}
	// A reserved machine identifier is transient.
	m = macDevice("m1")
	m.MachineIdentifierReserved = true
	if _, err := Place(req, []Device{m}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("reserved identifier: %v", err)
	}
	// An unsupported hardware model is permanent.
	snapReq := req
	snapReq.BaseImage = 0
	snapReq.Snapshot = &Snapshot{ID: "s", DeviceID: "m1", HostOS: ownsandv1.HostOs_HOST_OS_MACOS, Arch: ownsandv1.Arch_ARCH_ARM64, GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, MacOSHardwareModel: []byte("model-b")}
	if _, err := Place(snapReq, []Device{macDevice("m1")}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("hardware model: %v", err)
	}
}

func TestSnapshotPlacement(t *testing.T) {
	snap := &Snapshot{ID: "s", DeviceID: "d2", HostOS: ownsandv1.HostOs_HOST_OS_LINUX, Arch: ownsandv1.Arch_ARCH_AMD64, GuestOS: ownsandv1.GuestOs_GUEST_OS_LINUX,
		VCPUCount: 2, MemoryMiB: 1024, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, HasMemoryState: true}
	req := linuxRequest()
	req.BaseImage = 0
	req.Snapshot = snap
	d1, d2 := linuxDevice("d1"), linuxDevice("d2")
	d2.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 2}
	// Not exported: only the snapshot device.
	dec, err := Place(req, []Device{d1, d2}, now)
	if err != nil || dec.DeviceID != "d2" || !dec.MemoryRestore {
		t.Fatalf("local snapshot: %+v %v", dec, err)
	}
	// Different resources boot cold, still on d2.
	req.MemoryMiB = 2048
	dec, _ = Place(req, []Device{d1, d2}, now)
	if dec.DeviceID != "d2" || dec.MemoryRestore {
		t.Fatalf("cold boot: %+v", dec)
	}
	// Exported: the least loaded device, with an import.
	snap.Exported = true
	dec, _ = Place(req, []Device{d1, d2}, now)
	if dec.DeviceID != "d1" || !dec.NeedsImport {
		t.Fatalf("exported: %+v", dec)
	}
	// A device of another architecture never runs it.
	arm := linuxDevice("d0")
	arm.Arch = ownsandv1.Arch_ARCH_ARM64
	req.DeviceID = "d0"
	if _, err := Place(req, []Device{arm}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("arch mismatch: %v", err)
	}
}

func TestUpdateStates(t *testing.T) {
	d := linuxDevice("d1")
	d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("installing: %v", err)
	}
	d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE
	since := now.Add(-time.Hour)
	d.PendingIdleSince = &since
	if _, err := Place(linuxRequest(), []Device{d}, now); err != nil {
		t.Fatalf("pending idle for an hour: %v", err)
	}
	since = now.Add(-25 * time.Hour)
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("pending idle for a day: %v", err)
	}
	d.UpdateState = ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UP_TO_DATE
	d.VersionRevoked = true
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("revoked version: %v", err)
	}
}

func TestFreeDisk(t *testing.T) {
	d := linuxDevice("d1")
	d.Capabilities.FreeDiskBytes = 15 << 30
	d.UnreportedCreatingDiskGiB = 6
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("free disk minus unreported creates: %v", err)
	}
}
