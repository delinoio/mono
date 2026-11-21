// SPDX-License-Identifier: Apache-2.0

// Package hcsrt is the Windows sandbox runtime: one Hyper-V VM per sandbox,
// created through the Host Compute Service (HCS) API of computecore.dll. It
// boots the Linux base image with the HCS Linux direct kernel boot (console
// on COM1, root on the synthetic SCSI disk), reaches the guest helper over a
// Hyper-V socket (vsock in the Linux guest), and carries the Ethernet frames
// of OUTBOUND sandboxes over a second Hyper-V socket to a userspace network
// stack (package netstack), so the agent applies the network policy to
// every guest connection. No Hyper-V network adapter or virtual switch is
// used. Each compute system is created with
// ShouldTerminateOnLastHandleClosed and the agent holds its only handle, so
// the VM ends with the agent process.
//
// The agent must run elevated (as an Administrator or SYSTEM) on a host with
// Hyper-V enabled. Windows guests are not part of this version.
//
// See docs/cmds-ownsand-agent-foundation.md.
package hcsrt

import "log/slog"

// Vsock ports of the guest. A Linux guest sees Hyper-V sockets as AF_VSOCK;
// port N is the Hyper-V socket service ID {N:08x}-facb-11e6-bd58-64006a7986d3.
const (
	// GuestPort is the port of the guest helper.
	GuestPort = 1024
	// TunnelPort is the port of the network tunnel of OUTBOUND sandboxes
	// (see cmds/ownsand-guest/tunnel_linux.go).
	TunnelPort = 1025
)

// Owner is the HCS owner of the agent's compute systems. A restarted agent
// finds compute systems that outlived an earlier agent process by it.
const Owner = "ownsand-agent"

// systemIDPrefix prefixes the sandbox ID in the compute system ID.
const systemIDPrefix = "ownsand-"

// File names inside a VM directory and a snapshot directory.
const (
	diskFile    = "disk.vhd"
	consoleFile = "console.log"
	snapDisk    = "disk.vhd"
)

// CheckID names the hypervisor checks of the runtime.
const (
	checkHyperV  = "hyper-v"
	checkImage   = "linux-base-image"
	checkNetwork = "outbound-network"
)

// Options configure the runtime.
type Options struct {
	// Kernel and Rootfs are the Linux base image: the uncompressed x86_64
	// vmlinux and the ext4 root file system.
	Kernel string
	Rootfs string
	// VMsDir holds the VM directories (<state>/vms).
	VMsDir string
	Logger *slog.Logger
}
