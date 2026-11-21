// SPDX-License-Identifier: Apache-2.0

// Package vzrt is the macOS sandbox runtime: one Virtualization.framework
// VM per sandbox, through the github.com/Code-Hex/vz/v3 binding (cgo). It
// boots the Linux base image of the host architecture (the arm64 Image on
// Apple silicon) with the Linux boot loader, reaches the guest helper over
// the virtio socket device, and attaches OUTBOUND sandboxes to a userspace
// network stack (package netstack) through a file-handle network
// attachment, so the agent applies the network policy to every guest
// connection. The VMs run inside the agent process and end with it.
//
// The agent binary must be built with cgo on macOS and signed with the
// com.apple.security.virtualization entitlement (see
// cmds/ownsand-agent/entitlements.plist). A binary built without cgo (for
// example, cross-compiled on Linux in CI) reports a failing
// `virtualization-framework` check and rejects creates. macOS guests are
// not part of this version.
//
// See docs/cmds-ownsand-agent-foundation.md.
package vzrt

import "log/slog"

// GuestPort is the vsock port of the guest helper.
const GuestPort = 1024

// File names inside a VM directory and a snapshot directory.
const (
	diskFile      = "disk.img"
	consoleFile   = "console.log"
	snapState     = "vm.state"
	snapDisk      = "disk.img"
	snapMachineID = "machine-id"
)

// CheckID names the hypervisor checks of the runtime.
const (
	checkFramework   = "virtualization-framework"
	checkEntitlement = "virtualization-entitlement"
	checkImage       = "linux-base-image"
	checkNetwork     = "outbound-network"
)

// Options configure the runtime.
type Options struct {
	// Kernel and Rootfs are the Linux base image: the uncompressed kernel
	// of the host architecture and the ext4 root file system.
	Kernel string
	Rootfs string
	// MacOSImage is the macOS base image directory (images/macos; see
	// package images).
	MacOSImage string
	// VMsDir holds the VM directories (<state>/vms).
	VMsDir string
	Logger *slog.Logger
}
