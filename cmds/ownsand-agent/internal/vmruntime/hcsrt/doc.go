// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
)

// The HCS schema-v2 compute system document of a sandbox VM: the subset of
// https://learn.microsoft.com/en-us/virtualization/api/hcs/schemareference
// that the runtime sets. hcsshim defines these types in an internal
// package, so the runtime keeps its own copy. JSON names are the Go field
// names.

type schemaVersion struct {
	Major int
	Minor int
}

type computeSystemDoc struct {
	Owner         string
	SchemaVersion schemaVersion
	// ShouldTerminateOnLastHandleClosed binds the VM to the agent process,
	// which holds the only handle.
	ShouldTerminateOnLastHandleClosed bool
	VirtualMachine                    virtualMachineDoc
}

type virtualMachineDoc struct {
	// StopOnReset turns a guest reboot (panic=1) into a stop.
	StopOnReset     bool
	Chipset         chipsetDoc
	ComputeTopology topologyDoc
	Devices         devicesDoc
}

type chipsetDoc struct {
	LinuxKernelDirect linuxKernelDirectDoc
}

type linuxKernelDirectDoc struct {
	KernelFilePath string
	KernelCmdLine  string
}

type topologyDoc struct {
	Memory    memoryDoc
	Processor processorDoc
}

type memoryDoc struct {
	SizeInMB uint64
	// AllowOvercommit backs the guest memory with the page file instead of
	// locked host memory; EnableDeferredCommit charges commit on first
	// access. hcsshim uses the same defaults for its Linux utility VMs.
	AllowOvercommit      bool
	EnableDeferredCommit bool
}

type processorDoc struct {
	Count uint32
}

type devicesDoc struct {
	Scsi     map[string]scsiDoc
	HvSocket hvSocketDoc
	ComPorts map[string]comPortDoc `json:",omitempty"`
}

type scsiDoc struct {
	Attachments map[string]attachmentDoc
}

type attachmentDoc struct {
	Type string
	Path string
}

type hvSocketDoc struct {
	HvSocketConfig hvSocketConfigDoc
}

type hvSocketConfigDoc struct {
	DefaultBindSecurityDescriptor    string
	DefaultConnectSecurityDescriptor string
}

type comPortDoc struct {
	NamedPipe string
}

// hvSocketSDDL lets SYSTEM and the Administrators connect to (and bind)
// Hyper-V socket services of the VM.
const hvSocketSDDL = "D:P(A;;FA;;;SY)(A;;FA;;;BA)"

// vmSpec is the input of the compute system document.
type vmSpec struct {
	Kernel      string
	Disk        string
	ConsolePipe string
	VCPUs       int32
	MemoryMiB   int64
	// Tunnel is set for OUTBOUND sandboxes.
	Tunnel *netpolicy.Link
}

// kernelCmdLine returns the guest kernel command line.
func kernelCmdLine(tunnel *netpolicy.Link) string {
	// initcall_blacklist=virtio_vsock_init: the kernel also serves the
	// virtio hypervisors and has the virtio vsock transport built in, and
	// Linux registers only one guest-to-host vsock transport, the first to
	// initialize. Without this argument the virtio transport takes the slot
	// and the Hyper-V socket transport fails, as hcsshim also notes for its
	// Linux utility VMs. Remove it only with a kernel that lacks
	// CONFIG_VIRTIO_VSOCKETS.
	cmdline := "console=ttyS0 root=/dev/sda rw init=/sbin/init panic=1 initcall_blacklist=virtio_vsock_init"
	if tunnel != nil {
		cmdline += fmt.Sprintf(" ownsand.tunnel=%d,%s/%d,%s", TunnelPort, tunnel.Guest, tunnel.Bits, tunnel.Gateway)
	}
	return cmdline
}

// newComputeSystemDoc returns the compute system document of a sandbox VM.
// No network adapter is attached: OUTBOUND sandboxes use the tunnel.
func newComputeSystemDoc(s vmSpec) computeSystemDoc {
	// Hyper-V needs the memory size in whole 2 MiB pages.
	mem := uint64(s.MemoryMiB+1) &^ 1
	doc := computeSystemDoc{
		Owner:                             Owner,
		SchemaVersion:                     schemaVersion{Major: 2, Minor: 1},
		ShouldTerminateOnLastHandleClosed: true,
		VirtualMachine: virtualMachineDoc{
			StopOnReset: true,
			Chipset: chipsetDoc{LinuxKernelDirect: linuxKernelDirectDoc{
				KernelFilePath: s.Kernel,
				KernelCmdLine:  kernelCmdLine(s.Tunnel),
			}},
			ComputeTopology: topologyDoc{
				Memory:    memoryDoc{SizeInMB: mem, AllowOvercommit: true, EnableDeferredCommit: true},
				Processor: processorDoc{Count: uint32(s.VCPUs)},
			},
			Devices: devicesDoc{
				Scsi: map[string]scsiDoc{"0": {Attachments: map[string]attachmentDoc{
					"0": {Type: "VirtualDisk", Path: s.Disk},
				}}},
				HvSocket: hvSocketDoc{HvSocketConfig: hvSocketConfigDoc{
					DefaultBindSecurityDescriptor:    hvSocketSDDL,
					DefaultConnectSecurityDescriptor: hvSocketSDDL,
				}},
			},
		},
	}
	if s.ConsolePipe != "" {
		// "0" is COM1 (ttyS0).
		doc.VirtualMachine.Devices.ComPorts = map[string]comPortDoc{"0": {NamedPipe: s.ConsolePipe}}
	}
	return doc
}
