// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestMemoryRestoreEligible(t *testing.T) {
	res := func(vcpu int32, mem, disk int64) *ownsandv1.Resources {
		return &ownsandv1.Resources{VcpuCount: vcpu, MemoryMib: mem, DiskGib: disk}
	}
	base := func() Spec {
		return Spec{
			Resources:            res(2, 512, 2),
			NetworkMode:          ownsandv1.NetworkMode_NETWORK_MODE_NONE,
			MemoryRestoreAllowed: true,
			Snapshot:             &SnapshotSource{HasMemoryState: true, Resources: res(2, 512, 2)},
		}
	}
	tests := []struct {
		name      string
		mutate    func(*Spec)
		supported bool
		want      bool
	}{
		{"same resources", func(*Spec) {}, true, true},
		{"no snapshot", func(s *Spec) { s.Snapshot = nil }, true, false},
		{"disk-only snapshot", func(s *Spec) { s.Snapshot.HasMemoryState = false }, true, false},
		{"restore not allowed", func(s *Spec) { s.MemoryRestoreAllowed = false }, true, false},
		{"memory state unsupported", func(*Spec) {}, false, false},
		{"network enabled", func(s *Spec) { s.NetworkMode = ownsandv1.NetworkMode_NETWORK_MODE_UNSPECIFIED }, true, false},
		{"vcpu mismatch", func(s *Spec) { s.Resources = res(4, 512, 2) }, true, false},
		{"memory mismatch", func(s *Spec) { s.Resources = res(2, 1024, 2) }, true, false},
		{"larger disk", func(s *Spec) { s.Resources = res(2, 512, 3) }, true, false},
		{"smaller disk", func(s *Spec) { s.Resources = res(2, 512, 1) }, true, false},
		{"unknown snapshot disk and larger request", func(s *Spec) { s.Snapshot.Resources = res(2, 512, 0) }, true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec := base()
			tt.mutate(&spec)
			if got := MemoryRestoreEligible(spec, tt.supported); got != tt.want {
				t.Fatalf("MemoryRestoreEligible = %v, want %v", got, tt.want)
			}
		})
	}
}
