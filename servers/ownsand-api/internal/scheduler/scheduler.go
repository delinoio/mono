// SPDX-License-Identifier: FSL-1.1-ALv2

// Package scheduler places a create request on a device. It is a pure
// function of the request and the device records; the caller loads the
// records and reserves the resources in the transaction that creates the
// sandbox. The owning contract is the Scheduler and vCPU overcommit
// sections of docs/servers-ownsand-api-foundation.md.
package scheduler

import (
	"bytes"
	"fmt"
	"math"
	"slices"
	"sort"
	"time"

	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// MaxMacOSGuests is the Virtualization.framework limit of macOS guests on
// one Mac.
const MaxMacOSGuests = 2

// MaxPendingIdle is how long a device may report PENDING_IDLE and still get
// new sandboxes, so that a busy device can drain and install an update.
const MaxPendingIdle = 24 * time.Hour

// CheckOutboundNetwork is the check_id of the hypervisor check that an agent
// reports for OUTBOUND networking. The agent runtimes report the same id
// (see cmds/ownsand-agent/internal/vmruntime); keep them in step.
const CheckOutboundNetwork = "outbound-network"

// Snapshot describes a snapshot or template source.
type Snapshot struct {
	ID string
	// DeviceID holds the original local copy; empty when none does.
	DeviceID string
	// CopyDeviceIDs hold imported copies.
	CopyDeviceIDs []string
	Exported      bool
	HostOS        ownsandv1.HostOs
	Arch          ownsandv1.Arch
	GuestOS       ownsandv1.GuestOs
	VCPUCount     int32
	MemoryMiB     int64
	// DiskGiB is the persisted disk size of the snapshot. A memory-state
	// restore needs a request with exactly this disk size.
	DiskGiB        int64
	NetworkMode    ownsandv1.NetworkMode
	HasMemoryState bool
	// MacOSHardwareModel is the opaque hardware model of a macOS snapshot.
	MacOSHardwareModel []byte
}

// Request is a create request after validation.
type Request struct {
	VCPUCount   int32
	MemoryMiB   int64
	DiskGiB     int64
	GuestOS     ownsandv1.GuestOs
	NetworkMode ownsandv1.NetworkMode
	// BaseImage is set for a base image source.
	BaseImage ownsandv1.BaseImage
	// Snapshot is set for a snapshot or template source.
	Snapshot *Snapshot
	// Filters. DeviceID selects one device and no other.
	DeviceID string
	HostOS   ownsandv1.HostOs
	Arch     ownsandv1.Arch
}

// Device is one enrolled device with what the caller knows about it.
type Device struct {
	ID           string
	Online       bool
	LiveChannel  bool
	HostOS       ownsandv1.HostOs
	Arch         ownsandv1.Arch
	Capabilities *ownsandv1.DeviceCapabilities
	Limits       *ownsandv1.DeviceLimits
	// Allocation counts CREATING, RUNNING, PAUSED and DELETING sandboxes by
	// stored agent state, including macOS guests.
	Allocation *ownsandv1.DeviceAllocation
	// UnreportedCreatingDiskGiB is the disk of CREATING sandboxes that the
	// device has not reported yet; it is subtracted from the free disk.
	UnreportedCreatingDiskGiB int64
	UpdateState               ownsandv1.AgentUpdateState
	PendingIdleSince          *time.Time
	VersionRevoked            bool
	// MachineIdentifierReserved is set when a sandbox that still runs (or
	// may run) uses the macOS machine identifier of the request's snapshot.
	MachineIdentifierReserved bool
}

// Outcome classifies a device for a request.
type Outcome int

const (
	// Eligible: the device can run the request now.
	Eligible Outcome = iota
	// Transient: the device could run it after a transient condition ends.
	Transient
	// Permanent: the request breaks the device limits or the OS,
	// architecture, capability or source rules.
	Permanent
)

// Error is a placement failure with the Connect-compatible code.
type Error struct {
	Code   ownsandv1.ErrorCode
	Reason string
}

func (e *Error) Error() string { return e.Reason }

// Decision is a placement.
type Decision struct {
	DeviceID string
	// MemoryRestore allows a memory-state restore attempt.
	MemoryRestore bool
	// NeedsImport means the device must import the exported snapshot first.
	NeedsImport bool
}

// Evaluate classifies one device for the request and gives the reason when
// it is not eligible.
func Evaluate(req Request, d Device, now time.Time) (Outcome, string) {
	// Permanent rules first: they decide the code even when a transient
	// condition also holds.
	if req.HostOS != ownsandv1.HostOs_HOST_OS_UNSPECIFIED && d.HostOS != req.HostOS {
		return Permanent, "host OS does not match"
	}
	if req.Arch != ownsandv1.Arch_ARCH_UNSPECIFIED && d.Arch != req.Arch {
		return Permanent, "architecture does not match"
	}
	if req.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS && (d.HostOS != ownsandv1.HostOs_HOST_OS_MACOS || d.Arch != ownsandv1.Arch_ARCH_ARM64) {
		return Permanent, "macOS guests need an Apple silicon Mac"
	}
	if s := req.Snapshot; s != nil {
		if d.HostOS != s.HostOS || d.Arch != s.Arch {
			return Permanent, "the snapshot needs a device with the same OS and architecture"
		}
		if !holds(s, d.ID) && !s.Exported {
			return Permanent, "the snapshot is not on this device and is not exported"
		}
	}
	limits := d.Limits
	if limits == nil {
		limits = &ownsandv1.DeviceLimits{}
	}
	if req.VCPUCount > limits.GetVcpuCount() {
		return Permanent, "the request has more vCPUs than the device vCPU limit"
	}
	memory := EffectiveMemoryMiB(d.HostOS, req.MemoryMiB)
	if memory > limits.GetMemoryMib() {
		return Permanent, "the request needs more memory than the device limit"
	}
	if req.DiskGiB > limits.GetDiskGib() {
		return Permanent, "the request needs more disk than the device limit"
	}
	caps := d.Capabilities
	if caps != nil {
		if !slices.Contains(caps.GetGuestOses(), req.GuestOS) {
			return Permanent, "the device does not support the guest OS"
		}
		if req.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS && !caps.GetMacosGuestsSupported() {
			return Permanent, "the device does not support macOS guests"
		}
		if req.BaseImage != ownsandv1.BaseImage_BASE_IMAGE_UNSPECIFIED && !slices.Contains(caps.GetBuiltBaseImages(), req.BaseImage) {
			return Permanent, "the device has not built the base image"
		}
		// A device that holds the snapshot reports the model of its copy, so
		// its absence there is a mismatch. A device that needs an import does
		// not report the model before it imports the snapshot: its absence
		// is unknown, not unsupported (see modelUnknown). The agent checks
		// the model again before the download.
		if s := req.Snapshot; s != nil && holds(s, d.ID) && macOSModelMissing(s, caps) {
			return Permanent, "the device does not support the snapshot's macOS hardware model"
		}
		if req.NetworkMode == ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND && outboundNetworkFailed(caps) {
			return Permanent, "the device cannot provide outbound networking"
		}
	}

	// Transient rules.
	if !d.Online || !d.LiveChannel {
		return Transient, "the device is offline"
	}
	if caps == nil {
		return Transient, "the device has not reported its capabilities"
	}
	switch d.UpdateState {
	case ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING:
		return Transient, "the device is installing an agent update"
	case ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE:
		return Transient, "the device runs a revoked agent version"
	case ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE:
		if d.PendingIdleSince != nil && now.Sub(*d.PendingIdleSince) > MaxPendingIdle {
			return Transient, "the device is draining for an agent update"
		}
	}
	if d.VersionRevoked {
		return Transient, "the device runs a revoked agent version"
	}
	alloc := d.Allocation
	if alloc == nil {
		alloc = &ownsandv1.DeviceAllocation{}
	}
	if allocatedVCPU(alloc)+int64(req.VCPUCount) > validate.EffectiveVCPULimit(limits) {
		return Transient, "running sandboxes hold the vCPU capacity"
	}
	if alloc.GetMemoryMib()+memory > limits.GetMemoryMib() {
		return Transient, "running sandboxes hold the memory capacity"
	}
	if alloc.GetDiskGib()+req.DiskGiB > limits.GetDiskGib() {
		return Transient, "running sandboxes hold the disk capacity"
	}
	freeGiB := caps.GetFreeDiskBytes()>>30 - d.UnreportedCreatingDiskGiB
	if req.DiskGiB > freeGiB {
		return Transient, "the device has not enough free disk"
	}
	if req.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
		if alloc.GetMacosGuestCount() >= MaxMacOSGuests {
			return Transient, "the device runs two macOS guests"
		}
		if d.MachineIdentifierReserved {
			return Transient, "a running sandbox uses the macOS machine identifier of the snapshot"
		}
	}
	return Eligible, ""
}

// EffectiveMemoryMiB returns the memory that a sandbox of the requested size
// reserves on a device of the host OS. The Hyper-V runtime, the only runtime
// on a Windows host, rounds the memory of a VM up to an even number of MiB
// (see newComputeSystemDoc in the agent's hcsrt package), so an odd request
// reserves one MiB more there. Other hosts reserve the requested size. The
// sandbox record keeps the requested size; only the reservation changes.
// Keep this rule in step with the memory allocation sums in the api package.
func EffectiveMemoryMiB(hostOS ownsandv1.HostOs, requested int64) int64 {
	if hostOS == ownsandv1.HostOs_HOST_OS_WINDOWS && requested%2 != 0 && requested < math.MaxInt64 {
		return requested + 1
	}
	return requested
}

// macOSModelMissing reports whether the snapshot is a macOS snapshot with a
// hardware model that the capability report does not list.
func macOSModelMissing(s *Snapshot, caps *ownsandv1.DeviceCapabilities) bool {
	return s.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS && len(s.MacOSHardwareModel) > 0 &&
		!slices.ContainsFunc(caps.GetMacosHardwareModels(), func(m []byte) bool { return bytes.Equal(m, s.MacOSHardwareModel) })
}

// modelUnknown reports whether an eligible device would import an exported
// macOS snapshot whose hardware model the device does not report yet.
func modelUnknown(req Request, d Device) bool {
	s := req.Snapshot
	return s != nil && !holds(s, d.ID) && macOSModelMissing(s, d.Capabilities)
}

// outboundNetworkFailed reports whether the device explicitly reports a
// failed outbound network check. A report without the check does not fail.
func outboundNetworkFailed(caps *ownsandv1.DeviceCapabilities) bool {
	return slices.ContainsFunc(caps.GetHypervisorChecks(), func(c *ownsandv1.HypervisorCheck) bool {
		return c.GetCheckId() == CheckOutboundNetwork && !c.GetPassed()
	})
}

func holds(s *Snapshot, deviceID string) bool {
	return s.DeviceID == deviceID || slices.Contains(s.CopyDeviceIDs, deviceID)
}

// memoryRestore reports whether a memory-state restore may be attempted
// on the device.
func memoryRestore(req Request, deviceID string) bool {
	s := req.Snapshot
	return s != nil && s.HasMemoryState && s.DeviceID == deviceID &&
		req.VCPUCount == s.VCPUCount && req.MemoryMiB == s.MemoryMiB && req.DiskGiB == s.DiskGiB && req.NetworkMode == s.NetworkMode
}

// allocatedVCPU returns the exact allocated vCPUs. The 64-bit total is
// exact; vcpu_count is capped at 2147483647, so it serves only a record that
// lacks the total.
func allocatedVCPU(alloc *ownsandv1.DeviceAllocation) int64 {
	if alloc != nil && alloc.VcpuTotalCount != nil {
		return alloc.GetVcpuTotalCount()
	}
	return int64(alloc.GetVcpuCount())
}

// share returns the allocated share of the device's most constrained
// resource.
func share(d Device) float64 {
	alloc := d.Allocation
	if alloc == nil {
		alloc = &ownsandv1.DeviceAllocation{}
	}
	ratio := func(used, limit int64) float64 {
		if limit <= 0 {
			return 1
		}
		return float64(used) / float64(limit)
	}
	return max(
		ratio(allocatedVCPU(alloc), validate.EffectiveVCPULimit(d.Limits)),
		ratio(alloc.GetMemoryMib(), d.Limits.GetMemoryMib()),
		ratio(alloc.GetDiskGib(), d.Limits.GetDiskGib()),
	)
}

// Place selects a device. Among eligible devices it prefers the snapshot
// device when memory state can restore there, and otherwise a device that
// reports support for the macOS hardware model of an exported snapshot over
// one that does not report the model yet, then the device with the lowest
// allocated share of its most constrained resource, then the lowest device
// ID. With req.DeviceID it checks only that device. The
// error code is unavailable when a device could run the request after a
// transient condition ends and failed_precondition otherwise.
func Place(req Request, devices []Device, now time.Time) (Decision, error) {
	var eligible []Device
	transient := false
	reason := "no enrolled device"
	for _, d := range devices {
		if req.DeviceID != "" && d.ID != req.DeviceID {
			continue
		}
		outcome, why := Evaluate(req, d, now)
		switch outcome {
		case Eligible:
			eligible = append(eligible, d)
		case Transient:
			transient = true
			reason = why
		case Permanent:
			if !transient {
				reason = why
			}
		}
	}
	if len(eligible) == 0 {
		if req.DeviceID != "" && reason == "no enrolled device" {
			return Decision{}, &Error{Code: ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, Reason: "device not found"}
		}
		code := ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION
		if transient {
			code = ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE
		}
		return Decision{}, &Error{Code: code, Reason: fmt.Sprintf("no device can run the sandbox: %s", reason)}
	}
	for _, d := range eligible {
		if memoryRestore(req, d.ID) {
			return Decision{DeviceID: d.ID, MemoryRestore: true}, nil
		}
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		ui, uj := modelUnknown(req, eligible[i]), modelUnknown(req, eligible[j])
		if ui != uj {
			return uj
		}
		si, sj := share(eligible[i]), share(eligible[j])
		if si != sj {
			return si < sj
		}
		return eligible[i].ID < eligible[j].ID
	})
	chosen := eligible[0]
	dec := Decision{DeviceID: chosen.ID}
	if req.Snapshot != nil && !holds(req.Snapshot, chosen.ID) {
		dec.NeedsImport = true
	}
	return dec, nil
}
