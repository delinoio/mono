// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestDeviceRowsEffectiveVCPULimit(t *testing.T) {
	limits := func(vcpu, percent int32) *ownsandv1.DeviceLimits {
		return &ownsandv1.DeviceLimits{VcpuCount: vcpu, MemoryMib: 1024, DiskGib: 10, VcpuOvercommitPercent: percent}
	}
	for _, tc := range []struct {
		name   string
		device *ownsandv1.Device
		want   string
	}{
		{
			name:   "exact value above the int32 range",
			device: &ownsandv1.Device{Limits: limits(536870912, 400), EffectiveVcpuLimit: 2147483647, EffectiveVcpuLimitCount: 2147483648},
			want:   "0/2147483648 vCPU",
		},
		{
			name:   "in-range value",
			device: &ownsandv1.Device{Limits: limits(5, 150), EffectiveVcpuLimit: 7, EffectiveVcpuLimitCount: 7},
			want:   "0/7 vCPU",
		},
		{
			name:   "zero limits",
			device: &ownsandv1.Device{Limits: limits(0, 100)},
			want:   "0/0 vCPU",
		},
		{
			name:   "no limits",
			device: &ownsandv1.Device{},
			want:   "0/0 vCPU",
		},
	} {
		rows := deviceRows([]*ownsandv1.Device{tc.device})
		if len(rows) != 1 || !strings.HasPrefix(rows[0][4], tc.want) {
			t.Errorf("%s: allocation column = %v, want prefix %q", tc.name, rows, tc.want)
		}
	}
}

func TestDeviceRowsAllocatedVCPU(t *testing.T) {
	total := func(n int64) *int64 { return &n }
	for _, tc := range []struct {
		name  string
		alloc *ownsandv1.DeviceAllocation
		want  string
	}{
		{
			name:  "exact total above the int32 range",
			alloc: &ownsandv1.DeviceAllocation{VcpuCount: 2147483647, VcpuTotalCount: total(4294967294)},
			want:  "4294967294/0 vCPU",
		},
		{
			name:  "in-range total",
			alloc: &ownsandv1.DeviceAllocation{VcpuCount: 3, VcpuTotalCount: total(3)},
			want:  "3/0 vCPU",
		},
		{
			name:  "present zero total",
			alloc: &ownsandv1.DeviceAllocation{VcpuTotalCount: total(0)},
			want:  "0/0 vCPU",
		},
		{
			name:  "older server without the total",
			alloc: &ownsandv1.DeviceAllocation{VcpuCount: 7},
			want:  "7/0 vCPU",
		},
		{
			name:  "older server with a capped count",
			alloc: &ownsandv1.DeviceAllocation{VcpuCount: 2147483647},
			want:  "2147483647/0 vCPU",
		},
	} {
		rows := deviceRows([]*ownsandv1.Device{{Allocation: tc.alloc}})
		if len(rows) != 1 || !strings.HasPrefix(rows[0][4], tc.want) {
			t.Errorf("%s: allocation column = %v, want prefix %q", tc.name, rows, tc.want)
		}
	}
}
