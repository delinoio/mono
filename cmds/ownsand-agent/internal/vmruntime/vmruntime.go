// SPDX-License-Identifier: FSL-1.1-ALv2

// Package vmruntime defines the sandbox runtime interface of the agent. A
// runtime runs each sandbox in a VM (or, for the process test runtime, in a
// host process) and gives the agent a connection to the guest helper.
package vmruntime

import (
	"context"
	"errors"
	"net"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/config"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ErrUnsupported reports a runtime or an operation that this host or
// runtime does not provide.
var ErrUnsupported = errors.New("vmruntime: not supported")

// SnapshotSource is the snapshot that a new sandbox starts from.
type SnapshotSource struct {
	SnapshotID string
	// Dir holds the snapshot content that the runtime wrote.
	Dir            string
	HasMemoryState bool
	Resources      *ownsandv1.Resources
}

// Spec describes a sandbox to create.
type Spec struct {
	SandboxID string
	// Dir is the runtime-owned directory of the sandbox (disks, sockets,
	// memory state). The runtime creates it and Delete removes it.
	Dir         string
	Resources   *ownsandv1.Resources
	NetworkMode ownsandv1.NetworkMode
	GuestOS     ownsandv1.GuestOs
	BaseImage   ownsandv1.BaseImage
	// Snapshot is set when the sandbox starts from a snapshot.
	Snapshot *SnapshotSource
	// MemoryRestoreAllowed allows a memory-state restore when the snapshot
	// has one and the resources (vCPU count, memory and disk size) match; see
	// MemoryRestoreEligible.
	MemoryRestoreAllowed bool
}

// SnapshotInfo describes a snapshot that a runtime wrote.
type SnapshotInfo struct {
	HasMemoryState bool
	SizeBytes      int64
	// MacOSHardwareModel is the opaque Virtualization.framework hardware
	// model of a macOS guest snapshot, and nil for other guests.
	MacOSHardwareModel []byte
	// SourceState reports a state change of the source sandbox that the
	// snapshot attempt left behind, and is valid together with a non-nil
	// error. SANDBOX_STATE_UNSPECIFIED means that the attempt left the source
	// in the state that the manager recorded. SANDBOX_STATE_PAUSED means that
	// the runtime paused a running source for the capture and could not resume
	// it, so the VM is still paused while the manager records it as running;
	// the manager then records the sandbox as paused, so that a later explicit
	// resume reaches the runtime. No other value is valid.
	SourceState ownsandv1.SandboxState
}

// MacOSHardwareModelChecker is implemented by runtimes that run macOS
// guests. The agent uses it to report the hardware models of its local
// macOS snapshots and to check an import before it downloads.
type MacOSHardwareModelChecker interface {
	// MacOSHardwareModelSupported reports whether this host can run a macOS
	// guest with the hardware model.
	MacOSHardwareModelSupported(model []byte) bool
}

// HostCleaner is implemented by runtimes that create host resources outside
// the state directory: the Linux Firecracker runtime creates a tap device
// per OUTBOUND sandbox and the nftables table of the network policy. The
// device removal calls CleanupHost after it deleted every sandbox, because a
// removed agent exits for good and never runs the clean-up of the next agent
// start. CleanupHost returns the failures of the resources that it could not
// delete; a resource that is already absent is not a failure.
type HostCleaner interface {
	CleanupHost() error
}

// Capabilities are the runtime facts of the capability report.
type Capabilities struct {
	GuestOSes            []ownsandv1.GuestOs
	BaseImages           []ownsandv1.BaseImage
	MacOSGuestsSupported bool
	MacOSHardwareModels  [][]byte
	Checks               []*ownsandv1.HypervisorCheck
}

// VM is one running or paused sandbox VM.
type VM interface {
	SandboxID() string
	// Dial connects to the guest helper.
	Dial(ctx context.Context) (net.Conn, error)
	RestorePath() ownsandv1.SandboxRestorePath
	// Exited is closed when the VM process ends, also unexpectedly.
	Exited() <-chan struct{}
}

// Runtime runs sandbox VMs. Implementations must be safe for concurrent
// use; the agent serializes the commands of one sandbox.
type Runtime interface {
	Kind() config.RuntimeKind
	Capabilities(ctx context.Context) Capabilities
	// Create boots a VM. It returns when the guest helper answers
	// CheckReady, or with an error after which no VM is left.
	Create(ctx context.Context, spec Spec) (VM, error)
	// Pause stops the vCPUs and keeps the memory.
	Pause(ctx context.Context, vm VM) error
	Resume(ctx context.Context, vm VM) error
	// Snapshot writes the guest disk (and the memory state where the
	// runtime supports it) to dir. The VM keeps its state.
	Snapshot(ctx context.Context, vm VM, dir string) (SnapshotInfo, error)
	// Delete stops the VM of the sandbox, if any, and removes its
	// runtime files. It is idempotent.
	Delete(ctx context.Context, sandboxID string) error
	// Running lists the sandbox IDs whose VMs still run.
	Running(ctx context.Context) ([]string, error)
	// Close stops every VM.
	Close() error
}
