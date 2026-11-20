// Package runtime defines the hypervisor runtime interface of the agent. The
// platform package selects the implementation of the host OS. Every sandbox is a VM: no
// implementation runs sandbox code in a host process or container.
package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ErrVMNotFound reports a VM that does not exist.
var ErrVMNotFound = errors.New("vm does not exist")

// Unsupported returns the error for a guest or operation that the host
// runtime does not support. It maps to the Connect code unimplemented.
func Unsupported(what string) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New(what+" is not supported by this agent runtime"))
}

// Source is the disk source of a new VM: a base image or a local snapshot.
type Source struct {
	BaseImage  ownsandv1.BaseImage
	SnapshotID string
}

// VMSpec describes one sandbox VM.
type VMSpec struct {
	SandboxID string
	GuestOS   ownsandv1.GuestOs
	Source    Source
	VCPUs     int64
	MemoryMiB int64
	DiskGiB   int64
	Network   ownsandv1.NetworkMode
	// TryMemoryRestore asks for a memory-state restore of a snapshot source.
	// The runtime falls back to a cold boot of the snapshot disk when it
	// cannot read the saved state.
	TryMemoryRestore bool
}

// SnapshotInfo describes a snapshot that the runtime took.
type SnapshotInfo struct {
	SnapshotID     string
	SizeBytes      int64
	HasMemoryState bool
}

// VMInfo is a VM that the runtime still runs, tagged with its sandbox ID.
type VMInfo struct {
	SandboxID string
	Paused    bool
}

// Capabilities are the runtime part of the capability report.
type Capabilities struct {
	GuestOSes          []ownsandv1.GuestOs
	BuiltBaseImages    []ownsandv1.BaseImage
	MacOSSupported     bool
	MacOSHardwareModel []string
	// HypervisorChecks maps a check name to "ok" or a short failure text.
	HypervisorChecks map[string]string
}

// Runtime runs sandbox VMs on one host.
type Runtime interface {
	// Name is a short name for logs, for example "firecracker".
	Name() string
	// Capabilities probes the hypervisor and the local images.
	Capabilities(ctx context.Context) Capabilities
	// Create prepares the VM disk and configuration without starting it.
	Create(ctx context.Context, spec VMSpec) error
	// Start boots the VM, or restores its memory state, and reports which
	// restore path it used.
	Start(ctx context.Context, sandboxID string) (ownsandv1.RestorePath, error)
	// Pause stops the vCPUs and keeps the memory.
	Pause(ctx context.Context, sandboxID string) error
	// Resume continues a paused VM.
	Resume(ctx context.Context, sandboxID string) error
	// Stop stops the VM process and keeps its disk.
	Stop(ctx context.Context, sandboxID string) error
	// Delete stops the VM and deletes its disk and state. It is idempotent.
	Delete(ctx context.Context, sandboxID string) error
	// Snapshot captures the VM disk and, where supported, its memory state.
	Snapshot(ctx context.Context, sandboxID, snapshotID string) (SnapshotInfo, error)
	// DeleteSnapshot deletes a local snapshot. It is idempotent.
	DeleteSnapshot(ctx context.Context, snapshotID string) error
	// List returns the VMs that the runtime still runs for the agent.
	List(ctx context.Context) ([]VMInfo, error)
	// DialGuest connects to the guest helper of a running VM over the
	// hypervisor socket.
	DialGuest(ctx context.Context, sandboxID string) (io.ReadWriteCloser, error)
	// DeleteAll stops and deletes every VM, snapshot and locally built image.
	DeleteAll(ctx context.Context) error
	// Close releases runtime resources when the agent stops.
	Close() error
}

// Config configures the platform runtime.
type Config struct {
	StateDir string
	Logger   *slog.Logger
	// Passive opens the runtime without host side effects (no network
	// setup), for commands such as enroll and reset that do not run VMs.
	Passive bool
	// AllowUnsignedImage accepts an unsigned Linux base image. Development
	// and acceptance only.
	AllowUnsignedImage bool
}
