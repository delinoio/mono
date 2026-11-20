// SPDX-License-Identifier: FSL-1.1-ALv2

// Package scheduler decides which device runs a new sandbox. It is pure
// logic: the storage layer loads the facts of every candidate device inside
// the reservation transaction and calls Place. Contract:
// docs/servers-ownsand-api-foundation.md#scheduler.
package scheduler

import (
	"errors"
	"slices"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// PendingIdleLimit is how long a device may report PENDING_IDLE and still get
// new sandboxes.
const PendingIdleLimit = 24 * time.Hour

// MaxMacOSGuests is the Virtualization.framework limit of one Mac.
const MaxMacOSGuests = 2

// Resources are vCPU, memory and disk amounts.
type Resources struct {
	VCPUCount int64
	MemoryMiB int64
	DiskGiB   int64
}

// Snapshot holds the facts of a snapshot source.
type Snapshot struct {
	ID                     string
	DeviceID               string // empty when no local copy exists
	Exported               bool
	HostOS                 ownsandv1.HostOs
	Arch                   ownsandv1.Arch
	GuestOS                ownsandv1.GuestOs
	MacOSHardwareModel     string
	MacOSMachineIdentifier []byte
	HasMemoryState         bool
	Resources              Resources
	NetworkMode            ownsandv1.NetworkMode
}

// Request is a create request after validation.
type Request struct {
	GuestOS     ownsandv1.GuestOs
	BaseImage   ownsandv1.BaseImage // set for a base image source
	Snapshot    *Snapshot           // set for a snapshot or template source
	Resources   Resources
	NetworkMode ownsandv1.NetworkMode
	HostOS      *ownsandv1.HostOs
	Arch        *ownsandv1.Arch
	DeviceID    string
}

// Device holds the facts of one candidate device.
type Device struct {
	ID          string
	State       ownsandv1.DeviceState
	LiveChannel bool // a current agent channel on this instance whose state report was applied
	Caps        *ownsandv1.DeviceCapabilities
	Limits      Resources
	Allocated   Resources // creating, running, paused and deleting sandboxes
	// Disk of creating sandboxes that the agent has not reported yet.
	UnreportedCreatingDiskGiB int64
	// macOS guests in creating, running, paused or deleting state.
	MacOSGuests      int
	UpdateState      ownsandv1.AgentUpdateState
	PendingIdleSince *time.Time
	VersionRevoked   bool
	// The macOS machine identifier of the snapshot is in use by a live
	// sandbox (on any device).
	MachineIdentifierReserved bool
}

// Verdict is the evaluation of one device.
type Verdict struct {
	Eligible bool
	// Transient is set for an ineligible device that could run the request
	// after a transient condition ends.
	Transient     bool
	Reason        string
	MemoryRestore bool
}

func permanent(reason string) Verdict { return Verdict{Reason: reason} }
func transient(reason string) Verdict { return Verdict{Transient: true, Reason: reason} }

// Evaluate applies every eligibility rule. Permanent conditions are checked
// first, so a device that breaks one gets failed_precondition even when it is
// also offline.
func Evaluate(d Device, r Request, now time.Time) Verdict {
	caps := d.Caps
	if caps == nil {
		caps = &ownsandv1.DeviceCapabilities{}
	}
	if d.State == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
		return permanent("device removed")
	}
	if r.HostOS != nil && caps.GetHostOs() != *r.HostOS {
		return permanent("host OS does not match")
	}
	if r.Arch != nil && caps.GetArch() != *r.Arch {
		return permanent("architecture does not match")
	}
	if !slices.Contains(caps.GetGuestOses(), r.GuestOS) {
		return permanent("guest OS not supported")
	}
	if r.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
		if caps.GetHostOs() != ownsandv1.HostOs_HOST_OS_MACOS || caps.GetArch() != ownsandv1.Arch_ARCH_ARM64 || !caps.GetMacosGuestsSupported() {
			return permanent("macOS guests need an Apple silicon Mac")
		}
	}
	memoryRestore := false
	if s := r.Snapshot; s != nil {
		local := s.DeviceID != "" && s.DeviceID == d.ID
		if !local && !(s.Exported && caps.GetHostOs() == s.HostOS && caps.GetArch() == s.Arch) {
			return permanent("snapshot not available on this device")
		}
		if s.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS && !slices.Contains(caps.GetMacosHardwareModels(), s.MacOSHardwareModel) {
			return permanent("macOS hardware model not supported")
		}
		memoryRestore = local && s.HasMemoryState && r.Resources.VCPUCount == s.Resources.VCPUCount &&
			r.Resources.MemoryMiB == s.Resources.MemoryMiB && r.NetworkMode == s.NetworkMode
	} else if !slices.Contains(caps.GetBuiltBaseImages(), r.BaseImage) {
		return permanent("base image not built on this device")
	}
	if r.Resources.VCPUCount > d.Limits.VCPUCount || r.Resources.MemoryMiB > d.Limits.MemoryMiB || r.Resources.DiskGiB > d.Limits.DiskGiB {
		return permanent("request exceeds the device limits")
	}

	if d.State != ownsandv1.DeviceState_DEVICE_STATE_ONLINE || !d.LiveChannel {
		return transient("device offline")
	}
	if d.VersionRevoked || d.UpdateState == ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_REVOKED_PENDING_IDLE {
		return transient("agent version revoked")
	}
	if d.UpdateState == ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_INSTALLING {
		return transient("agent update installing")
	}
	if d.UpdateState == ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE && d.PendingIdleSince != nil && now.Sub(*d.PendingIdleSince) > PendingIdleLimit {
		return transient("device draining for an agent update")
	}
	if d.Allocated.VCPUCount+r.Resources.VCPUCount > d.Limits.VCPUCount ||
		d.Allocated.MemoryMiB+r.Resources.MemoryMiB > d.Limits.MemoryMiB ||
		d.Allocated.DiskGiB+r.Resources.DiskGiB > d.Limits.DiskGiB {
		return transient("capacity held by running sandboxes")
	}
	free := caps.GetFreeDiskBytes() - d.UnreportedCreatingDiskGiB<<30
	if r.Resources.DiskGiB<<30 > free {
		return transient("not enough free disk")
	}
	if r.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
		if d.MacOSGuests >= MaxMacOSGuests {
			return transient("macOS guest limit reached")
		}
		if d.MachineIdentifierReserved {
			return transient("macOS machine identifier in use")
		}
	}
	return Verdict{Eligible: true, MemoryRestore: memoryRestore}
}

// share is the allocated share of the device's most constrained resource.
func share(d Device) float64 {
	frac := func(a, l int64) float64 {
		if l <= 0 {
			return 1
		}
		return float64(a) / float64(l)
	}
	return max(frac(d.Allocated.VCPUCount, d.Limits.VCPUCount), frac(d.Allocated.MemoryMiB, d.Limits.MemoryMiB), frac(d.Allocated.DiskGiB, d.Limits.DiskGiB))
}

// Errors of Place.
var (
	ErrUnavailable        = connect.NewError(connect.CodeUnavailable, errors.New("no device can run the sandbox now; retry later"))
	ErrFailedPrecondition = connect.NewError(connect.CodeFailedPrecondition, errors.New("no device can run the sandbox"))
)

// Placement is the chosen device.
type Placement struct {
	DeviceID      string
	MemoryRestore bool
}

// Place selects a device. With r.DeviceID set, only that device is
// considered. It returns ErrUnavailable when at least one device fails only
// transient conditions, and ErrFailedPrecondition otherwise.
func Place(devices []Device, r Request, now time.Time) (Placement, error) {
	var best *Device
	var bestVerdict Verdict
	anyTransient := false
	better := func(d Device, v Verdict) bool {
		if best == nil {
			return true
		}
		// Prefer the snapshot device when memory state can restore there.
		if v.MemoryRestore != bestVerdict.MemoryRestore {
			return v.MemoryRestore
		}
		sd, sb := share(d), share(*best)
		if sd != sb {
			return sd < sb
		}
		return d.ID < best.ID
	}
	for i := range devices {
		d := devices[i]
		if r.DeviceID != "" && d.ID != r.DeviceID {
			continue
		}
		v := Evaluate(d, r, now)
		if !v.Eligible {
			anyTransient = anyTransient || v.Transient
			continue
		}
		if better(d, v) {
			best, bestVerdict = &devices[i], v
		}
	}
	if best == nil {
		if anyTransient {
			return Placement{}, ErrUnavailable
		}
		return Placement{}, ErrFailedPrecondition
	}
	return Placement{DeviceID: best.ID, MemoryRestore: bestVerdict.MemoryRestore}, nil
}
