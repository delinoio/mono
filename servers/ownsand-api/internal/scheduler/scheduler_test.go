// SPDX-License-Identifier: FSL-1.1-ALv2

package scheduler

import (
	"errors"
	"math"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

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

func TestAllocatedVCPUBeyondInt32(t *testing.T) {
	const total = 2 * int64(math.MaxInt32)
	// The exact total decides capacity: the capped int32 field alone would
	// look like 2147483647 and leave room under a 4294967294 limit.
	d := linuxDevice("d1")
	d.Limits.VcpuCount = math.MaxInt32
	d.Limits.VcpuOvercommitPercent = 200 // effective 4294967294
	d.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: math.MaxInt32, VcpuTotalCount: proto.Int64(total)}
	if _, err := Place(linuxRequest(), []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("exact allocation at the limit: %v", err)
	}
	d.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: math.MaxInt32, VcpuTotalCount: proto.Int64(total - 2)}
	if _, err := Place(linuxRequest(), []Device{d}, now); err != nil {
		t.Fatalf("exact allocation with room: %v", err)
	}
	// A record without the total falls back to the 32-bit count.
	d.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: math.MaxInt32}
	if _, err := Place(linuxRequest(), []Device{d}, now); err != nil {
		t.Fatalf("allocation without a total: %v", err)
	}
	// Ranking uses the exact value: the fuller device ranks last.
	a, b := linuxDevice("a"), linuxDevice("b")
	for _, x := range []*Device{&a, &b} {
		x.Limits.VcpuCount = math.MaxInt32
		x.Limits.VcpuOvercommitPercent = 400
	}
	a.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: math.MaxInt32, VcpuTotalCount: proto.Int64(total)}
	b.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: math.MaxInt32, VcpuTotalCount: proto.Int64(math.MaxInt32)}
	dec, err := Place(linuxRequest(), []Device{a, b}, now)
	if err != nil || dec.DeviceID != "b" {
		t.Fatalf("ranking = %v, %v", dec, err)
	}
}

func windowsDevice(id string) Device {
	d := linuxDevice(id)
	d.HostOS = ownsandv1.HostOs_HOST_OS_WINDOWS
	return d
}

func TestEffectiveMemoryMiB(t *testing.T) {
	for _, tc := range []struct {
		host      ownsandv1.HostOs
		requested int64
		want      int64
	}{
		{ownsandv1.HostOs_HOST_OS_WINDOWS, 511, 512},
		{ownsandv1.HostOs_HOST_OS_WINDOWS, 512, 512},
		{ownsandv1.HostOs_HOST_OS_WINDOWS, 1, 2},
		{ownsandv1.HostOs_HOST_OS_WINDOWS, math.MaxInt64, math.MaxInt64}, // no overflow
		{ownsandv1.HostOs_HOST_OS_LINUX, 511, 511},
		{ownsandv1.HostOs_HOST_OS_MACOS, 511, 511},
		{ownsandv1.HostOs_HOST_OS_UNSPECIFIED, 511, 511},
	} {
		if got := EffectiveMemoryMiB(tc.host, tc.requested); got != tc.want {
			t.Errorf("EffectiveMemoryMiB(%v, %d) = %d, want %d", tc.host, tc.requested, got, tc.want)
		}
	}
}

// TestHyperVMemoryRounding checks that a Windows device compares the even
// rounded memory with its limit, and other hosts the requested memory.
func TestHyperVMemoryRounding(t *testing.T) {
	req := linuxRequest()
	req.MemoryMiB = 4095 // the device limit is 4096

	// Rounds to 4096: fits exactly.
	if _, err := Place(req, []Device{windowsDevice("w")}, now); err != nil {
		t.Fatalf("odd request below an even limit: %v", err)
	}
	// Rounds to 4096 and exceeds an odd limit of 4095: permanent.
	d := windowsDevice("w")
	d.Limits.MemoryMib = 4095
	if _, err := Place(req, []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("odd request at an odd limit: %v", err)
	}
	// The same request and limit on Linux keep the requested size.
	l := linuxDevice("l")
	l.Limits.MemoryMib = 4095
	if _, err := Place(req, []Device{l}, now); err != nil {
		t.Fatalf("Linux placement: %v", err)
	}
	// Transient rule: the allocation (already rounded) plus the rounded
	// request must fit.
	d = windowsDevice("w")
	d.Allocation = &ownsandv1.DeviceAllocation{MemoryMib: 1}
	if _, err := Place(req, []Device{d}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
		t.Fatalf("allocation plus rounded request above the limit: %v", err)
	}
	// An even request is not changed.
	even := linuxRequest()
	even.MemoryMiB = 4096
	if _, err := Place(even, []Device{windowsDevice("w")}, now); err != nil {
		t.Fatalf("even request at the limit: %v", err)
	}
}

// outboundNetworkDevice reports the outbound-network check as passed or
// failed.
func outboundNetworkDevice(id string, passed bool) Device {
	d := linuxDevice(id)
	d.Capabilities.HypervisorChecks = []*ownsandv1.HypervisorCheck{
		{CheckId: "linux-base-image", Passed: true},
		{CheckId: CheckOutboundNetwork, Passed: passed, Detail: "OUTBOUND sandboxes need CAP_NET_ADMIN, ip and nft"},
	}
	return d
}

// TestOutboundNetworkCheck checks that a device that reports a failed
// outbound-network check never gets an OUTBOUND sandbox, whatever its load.
func TestOutboundNetworkCheck(t *testing.T) {
	failed, passed := outboundNetworkDevice("a-failed", false), outboundNetworkDevice("b-passed", true)
	// The failed device has the lower load and the lower ID, and still loses.
	passed.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 2}
	dec, err := Place(linuxRequest(), []Device{failed, passed}, now)
	if err != nil || dec.DeviceID != "b-passed" {
		t.Fatalf("auto placement = %+v, %v", dec, err)
	}

	// A lone failed device and a pinned failed device are permanent mismatches.
	if _, err := Place(linuxRequest(), []Device{failed}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("lone failed device: %v", err)
	}
	pinned := linuxRequest()
	pinned.DeviceID = "a-failed"
	if _, err := Place(pinned, []Device{failed, passed}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("pinned failed device: %v", err)
	}
	// The permanent rule decides the code even when a transient condition
	// also holds.
	offline := outboundNetworkDevice("offline", false)
	offline.Online = false
	if _, err := Place(linuxRequest(), []Device{offline}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("offline failed device: %v", err)
	}

	// NONE requests do not need the check.
	none := linuxRequest()
	none.NetworkMode = ownsandv1.NetworkMode_NETWORK_MODE_NONE
	dec, err = Place(none, []Device{failed, passed}, now)
	if err != nil || dec.DeviceID != "a-failed" {
		t.Fatalf("NONE request = %+v, %v", dec, err)
	}

	// A report without the check, and an empty report, stay eligible.
	omitted := linuxDevice("omitted")
	if _, err := Place(linuxRequest(), []Device{omitted}, now); err != nil {
		t.Fatalf("report without the check: %v", err)
	}
	other := linuxDevice("other")
	other.Capabilities.HypervisorChecks = []*ownsandv1.HypervisorCheck{{CheckId: "linux-base-image", Passed: false}}
	if _, err := Place(linuxRequest(), []Device{other}, now); err != nil {
		t.Fatalf("another failed check: %v", err)
	}
}

// macSnapshot is an exported macOS snapshot that mac "holder" took.
func macSnapshot(exported bool) *Snapshot {
	return &Snapshot{ID: "s", DeviceID: "holder", Exported: exported, HostOS: ownsandv1.HostOs_HOST_OS_MACOS, Arch: ownsandv1.Arch_ARCH_ARM64,
		GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, MacOSHardwareModel: []byte("model-b")}
}

func macSnapshotRequest(s *Snapshot) Request {
	return Request{VCPUCount: 4, MemoryMiB: 8192, DiskGiB: 80, GuestOS: ownsandv1.GuestOs_GUEST_OS_MACOS, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE, Snapshot: s}
}

// TestMacOSSnapshotUnknownModel checks that a destination that omits the
// hardware model of an exported macOS snapshot is unknown, not unsupported:
// it is eligible and imports first, an automatic placement prefers a device
// that reports the model, and a device that holds the snapshot keeps the
// membership rule.
func TestMacOSSnapshotUnknownModel(t *testing.T) {
	snap := macSnapshot(true)
	req := macSnapshotRequest(snap)
	holder := macDevice("holder") // holds the snapshot and lists model-a only
	unknown := macDevice("a-unknown")
	reporting := macDevice("b-reporting")
	reporting.Capabilities.MacosHardwareModels = [][]byte{[]byte("model-a"), []byte("model-b")}

	// An explicit destination that omits the model is eligible and imports.
	pinned := req
	pinned.DeviceID = "a-unknown"
	dec, err := Place(pinned, []Device{holder, unknown, reporting}, now)
	if err != nil || dec.DeviceID != "a-unknown" || !dec.NeedsImport {
		t.Fatalf("explicit unknown device = %+v, %v", dec, err)
	}

	// The unknown device has the lowest load and the lowest ID, and still
	// loses to a device that reports support.
	reporting.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 8, MemoryMib: 16384, DiskGib: 160}
	dec, err = Place(req, []Device{unknown, reporting}, now)
	if err != nil || dec.DeviceID != "b-reporting" || !dec.NeedsImport {
		t.Fatalf("preference for the reporting device = %+v, %v", dec, err)
	}

	// Without a qualifying device, unknown devices run it, ranked by load
	// and then by ID.
	other := macDevice("c-unknown")
	other.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 2}
	unknown.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 4}
	dec, err = Place(req, []Device{unknown, other}, now)
	if err != nil || dec.DeviceID != "c-unknown" || !dec.NeedsImport {
		t.Fatalf("unknown devices only = %+v, %v", dec, err)
	}
	// A transient condition on the reporting device leaves the unknown device.
	busy := reporting
	busy.Online = false
	dec, err = Place(req, []Device{busy, unknown}, now)
	if err != nil || dec.DeviceID != "a-unknown" {
		t.Fatalf("reporting device offline = %+v, %v", dec, err)
	}

	// The device that holds the snapshot keeps the membership rule: it
	// lists model-a only, so it is a permanent mismatch for model-b.
	local := req
	local.DeviceID = "holder"
	if _, err := Place(local, []Device{holder}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("holder without the model: %v", err)
	}
	// So does a device with an imported copy.
	copied := macSnapshot(true)
	copied.CopyDeviceIDs = []string{"a-unknown"}
	if _, err := Place(macSnapshotRequest(copied), []Device{unknown}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("copy holder without the model: %v", err)
	}

	// An unexported snapshot runs only where it is, and a request without
	// a model has no unknown devices.
	if _, err := Place(macSnapshotRequest(macSnapshot(false)), []Device{unknown}, now); code(err) != ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION {
		t.Fatalf("unexported snapshot on another device: %v", err)
	}
	noModel := macSnapshot(true)
	noModel.MacOSHardwareModel = nil
	dec, err = Place(macSnapshotRequest(noModel), []Device{unknown, reporting}, now)
	if err != nil || dec.DeviceID != "a-unknown" {
		t.Fatalf("snapshot without a model = %+v, %v", dec, err)
	}
}
