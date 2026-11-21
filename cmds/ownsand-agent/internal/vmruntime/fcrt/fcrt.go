// SPDX-License-Identifier: FSL-1.1-ALv2

// Package fcrt is the Linux sandbox runtime: one Firecracker microVM per
// sandbox on KVM. The agent talks to the Firecracker API socket with a
// small HTTP client (no SDK), boots the Linux base image (an uncompressed
// vmlinux and an ext4 root file system whose /sbin/init is the guest
// helper), reaches the guest helper over the Firecracker vsock device and
// attaches OUTBOUND sandboxes to a tap device under the nftables policy of
// package netpolicy. See docs/cmds-ownsand-agent-foundation.md.
package fcrt

import (
	"log/slog"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
)

// PinnedVersion is the Firecracker release that this agent version is
// tested with.
const PinnedVersion = "v1.17.0"

// GuestPort is the vsock port of the guest helper.
const GuestPort = 1024

// guestCID is the vsock CID of every guest; each VM has its own vsock
// device, so the CID does not need to be unique.
const guestCID = 3

// File names inside a VM directory and a snapshot directory. Firecracker
// runs with the VM directory as its working directory and gets relative
// paths, so the paths inside a memory snapshot stay valid in another VM
// directory.
const (
	apiSocket     = "api.sock"
	vsockSocket   = "v.sock"
	diskFile      = "rootfs.ext4"
	logFile       = "firecracker.log"
	snapState     = "vm.state"
	snapMemory    = "memory"
	snapDisk      = "disk.ext4"
	idPrefix      = "ownsand-"
	maxSocketPath = 100
)

// Options configure the runtime.
type Options struct {
	// Binary is the firecracker executable.
	Binary string
	// Kernel and Rootfs are the Linux base image.
	Kernel string
	Rootfs string
	// VMsDir holds the VM directories (<state>/vms).
	VMsDir string
	// Network attaches OUTBOUND sandboxes; nil disables OUTBOUND.
	Network *netpolicy.Manager
	Logger  *slog.Logger
}
