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
// Windows guests boot the Windows base image (a dynamic VHDX that
// `ownsand-agent image build` makes, see package imagebuild) with the
// Hyper-V UEFI firmware; their guest helper is a LocalSystem service that
// listens on the Hyper-V socket service ID of vsock port GuestPort. Windows
// guests run only with network mode NONE in this version.
//
// The agent must run elevated (as an Administrator or SYSTEM) on a host with
// Hyper-V enabled.
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

// File names inside a VM directory and a snapshot directory. A Windows
// snapshot also holds a copy of the image.json of its base image, so its
// resource minimums outlive an image rebuild.
const (
	diskFile        = "disk.vhd"
	windowsDiskFile = "disk.vhdx"
	consoleFile     = "console.log"
	snapDisk        = "disk.vhd"
	snapWindowsDisk = "disk.vhdx"
)

// CheckID names the hypervisor checks of the runtime.
const (
	checkHyperV       = "hyper-v"
	checkImage        = "linux-base-image"
	checkWindowsImage = "windows-base-image"
	checkNetwork      = "outbound-network"
)

// Windows guest helper installation of the Windows base image. The image
// build installs the helper at WindowsGuestBinary and registers it as the
// auto-start LocalSystem service WindowsGuestService with
// WindowsGuestListen, the Hyper-V socket service ID that the runtime dials
// (winio.VsockServiceID(GuestPort)).
const (
	WindowsGuestBinary  = `C:\Program Files\OwnSand\ownsand-guest.exe`
	WindowsGuestService = "ownsand-guest"
	WindowsGuestListen  = "hvsock://00000400-facb-11e6-bd58-64006a7986d3"
)

// Options configure the runtime.
type Options struct {
	// Kernel and Rootfs are the Linux base image: the uncompressed x86_64
	// vmlinux and the ext4 root file system.
	Kernel string
	Rootfs string
	// WindowsImage is the Windows base image directory (images/windows;
	// see package images).
	WindowsImage string
	// VMsDir holds the VM directories (<state>/vms).
	VMsDir string
	Logger *slog.Logger
}
