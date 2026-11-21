// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"encoding/json"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
)

func TestComputeSystemDoc(t *testing.T) {
	link, err := netpolicy.LinkFor(1)
	if err != nil {
		t.Fatal(err)
	}
	doc := newComputeSystemDoc(vmSpec{Kernel: `C:\img\vmlinux`, Disk: `C:\vms\a\disk.vhd`,
		ConsolePipe: `\\.\pipe\ownsand-a-com1`, VCPUs: 2, MemoryMiB: 511, Tunnel: &link})
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	get := func(path ...string) any {
		var v any = got
		for _, p := range path {
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("%v: not an object in %s", path, b)
			}
			v = m[p]
		}
		return v
	}
	for _, tc := range []struct {
		path []string
		want any
	}{
		{[]string{"Owner"}, "ownsand-agent"},
		{[]string{"SchemaVersion", "Major"}, 2.0},
		{[]string{"ShouldTerminateOnLastHandleClosed"}, true},
		{[]string{"VirtualMachine", "StopOnReset"}, true},
		{[]string{"VirtualMachine", "Chipset", "LinuxKernelDirect", "KernelFilePath"}, `C:\img\vmlinux`},
		{[]string{"VirtualMachine", "Chipset", "LinuxKernelDirect", "KernelCmdLine"},
			"console=ttyS0 root=/dev/sda rw init=/sbin/init panic=1 initcall_blacklist=virtio_vsock_init ownsand.tunnel=1025,172.30.0.6/30,172.30.0.5"},
		{[]string{"VirtualMachine", "ComputeTopology", "Memory", "SizeInMB"}, 512.0},
		{[]string{"VirtualMachine", "ComputeTopology", "Processor", "Count"}, 2.0},
		{[]string{"VirtualMachine", "Devices", "Scsi", "0", "Attachments", "0", "Type"}, "VirtualDisk"},
		{[]string{"VirtualMachine", "Devices", "Scsi", "0", "Attachments", "0", "Path"}, `C:\vms\a\disk.vhd`},
		{[]string{"VirtualMachine", "Devices", "HvSocket", "HvSocketConfig", "DefaultConnectSecurityDescriptor"}, hvSocketSDDL},
		{[]string{"VirtualMachine", "Devices", "ComPorts", "0", "NamedPipe"}, `\\.\pipe\ownsand-a-com1`},
	} {
		if v := get(tc.path...); v != tc.want {
			t.Errorf("%v = %v, want %v", tc.path, v, tc.want)
		}
	}
	if get("VirtualMachine", "Devices", "NetworkAdapters") != nil {
		t.Error("the VM has a network adapter")
	}
	if cmd := kernelCmdLine(nil); cmd != "console=ttyS0 root=/dev/sda rw init=/sbin/init panic=1 initcall_blacklist=virtio_vsock_init" {
		t.Errorf("NONE command line %q", cmd)
	}
}
