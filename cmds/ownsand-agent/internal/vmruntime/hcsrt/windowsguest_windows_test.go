// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"errors"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/images"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestCheckWindowsResources(t *testing.T) {
	m := images.Metadata{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, MinimumCPUCount: 2, MinimumMemoryMiB: 2048}
	for _, tc := range []struct {
		vcpu int32
		mem  int64
		ok   bool
	}{{2, 2048, true}, {4, 8192, true}, {1, 4096, false}, {2, 2047, false}} {
		err := checkWindowsResources(&ownsandv1.Resources{VcpuCount: tc.vcpu, MemoryMib: tc.mem}, m)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, vmruntime.ErrUnsupported)) {
			t.Errorf("%d vCPUs %d MiB: %v", tc.vcpu, tc.mem, err)
		}
	}
}

func TestWindowsOutboundUnsupported(t *testing.T) {
	r := &Runtime{opts: Options{WindowsImage: t.TempDir()}}
	err := r.checkWindowsSpec(vmruntime.Spec{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND})
	if !errors.Is(err, vmruntime.ErrUnsupported) {
		t.Fatalf("OUTBOUND Windows sandbox: %v", err)
	}
	err = r.checkWindowsSpec(vmruntime.Spec{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, NetworkMode: ownsandv1.NetworkMode_NETWORK_MODE_NONE})
	if !errors.Is(err, vmruntime.ErrUnsupported) {
		t.Fatalf("Windows sandbox without an image: %v", err)
	}
}
