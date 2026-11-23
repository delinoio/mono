// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"errors"
	"strings"
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
	// The image directory stays empty, so only the order of the checks tells the two cases apart:
	// the OUTBOUND guard must run before the missing-disk check.
	r := &Runtime{opts: Options{WindowsImage: t.TempDir()}}
	for _, tc := range []struct {
		name string
		mode ownsandv1.NetworkMode
		want string
	}{
		{"outbound", ownsandv1.NetworkMode_NETWORK_MODE_OUTBOUND, "Windows sandboxes run only with network mode NONE in this version"},
		{"none without an image", ownsandv1.NetworkMode_NETWORK_MODE_NONE, "the Windows base image is not built"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := r.checkWindowsSpec(vmruntime.Spec{GuestOS: ownsandv1.GuestOs_GUEST_OS_WINDOWS, NetworkMode: tc.mode})
			if !errors.Is(err, vmruntime.ErrUnsupported) {
				t.Fatalf("errors.Is(err, ErrUnsupported) is false: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error does not contain %q: %v", tc.want, err)
			}
		})
	}
}
