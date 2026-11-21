// SPDX-License-Identifier: Apache-2.0

package sandboxes

import (
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Record is the persisted record of one sandbox.
type Record struct {
	SandboxID   string                       `json:"sandbox_id"`
	State       ownsandv1.SandboxState       `json:"state"`
	EndReason   ownsandv1.SandboxEndReason   `json:"end_reason,omitempty"`
	RestorePath ownsandv1.SandboxRestorePath `json:"restore_path,omitempty"`
	// Revision is the counter of the agent's state epoch.
	Revision uint64 `json:"revision"`
	// Acked is true when the control plane acknowledged Revision.
	Acked       bool                  `json:"acked"`
	VCPUCount   int32                 `json:"vcpu_count"`
	MemoryMiB   int64                 `json:"memory_mib"`
	DiskGiB     int64                 `json:"disk_gib"`
	NetworkMode ownsandv1.NetworkMode `json:"network_mode"`
	GuestOS     ownsandv1.GuestOs     `json:"guest_os"`
	BaseImage   ownsandv1.BaseImage   `json:"base_image,omitempty"`
	SnapshotID  string                `json:"snapshot_id,omitempty"`
	CreateTime  time.Time             `json:"create_time"`
	MaxDeadline time.Time             `json:"max_deadline"`
	IdleTTL     time.Duration         `json:"idle_ttl"`
	// IdleDeadline is not enforced while the sandbox is paused.
	IdleDeadline time.Time `json:"idle_deadline"`
	// DeleteMarker: a delete arrived before the create, which the agent
	// then rejects.
	DeleteMarker bool `json:"delete_marker,omitempty"`
}

// Live reports whether the sandbox has a VM or may get one.
func (r *Record) Live() bool {
	switch r.State {
	case ownsandv1.SandboxState_SANDBOX_STATE_CREATING,
		ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
		ownsandv1.SandboxState_SANDBOX_STATE_PAUSED,
		ownsandv1.SandboxState_SANDBOX_STATE_DELETING:
		return true
	}
	return false
}

// Terminal reports whether the sandbox ended.
func (r *Record) Terminal() bool {
	return r.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || r.State == ownsandv1.SandboxState_SANDBOX_STATE_FAILED
}

// Entry returns the state entry of the record.
func (r *Record) Entry(epoch string) *ownsandv1.SandboxStateEntry {
	e := &ownsandv1.SandboxStateEntry{
		SandboxId:   r.SandboxID,
		State:       r.State,
		RestorePath: r.RestorePath,
		Revision:    &ownsandv1.SandboxRevision{Epoch: epoch, Counter: r.Revision},
	}
	if r.Terminal() {
		e.EndReason = r.EndReason
	}
	if !r.IdleDeadline.IsZero() {
		e.IdleDeadline = timestamppb.New(r.IdleDeadline)
	}
	return e
}

// ttlReason returns the end reason of the earlier passed deadline at now,
// or UNSPECIFIED when no deadline passed. MAX_TTL_EXPIRED wins when both
// deadlines are equal. A paused sandbox has no idle deadline.
//
// A sandbox in DELETING whose failed end attempt recorded a TTL reason keeps
// that reason. The state change to DELETING hides that the sandbox was
// paused, so recomputing the reason would apply its stale idle deadline and
// could replace MAX_TTL_EXPIRED with IDLE_TTL_EXPIRED on a retry.
func (r *Record) ttlReason(now time.Time) ownsandv1.SandboxEndReason {
	if r.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING {
		switch r.EndReason {
		case ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED,
			ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED:
			return r.EndReason
		}
	}
	maxPassed := !r.MaxDeadline.IsZero() && !now.Before(r.MaxDeadline)
	idleApplies := r.State != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED && !r.IdleDeadline.IsZero()
	idlePassed := idleApplies && !now.Before(r.IdleDeadline)
	switch {
	case maxPassed && idlePassed:
		if r.IdleDeadline.Before(r.MaxDeadline) {
			return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
		}
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED
	case maxPassed:
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED
	case idlePassed:
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
	}
	return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED
}
