// SPDX-License-Identifier: FSL-1.1-ALv2

package scheduler

import (
	"strings"
	"testing"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestPlaceTieBreaksOnLowestDeviceID covers the final tie-break of Place:
// when eligible devices have equal allocation shares, the lowest device ID
// wins regardless of the order in which the devices are presented. Each case
// is run with the lower ID first and last, so neither the stable sort alone
// nor the input order can produce the expected result.
func TestPlaceTieBreaksOnLowestDeviceID(t *testing.T) {
	tests := []struct {
		name  string
		build func() map[string]Device
		want  string
		// order lists device IDs in the order they are presented to Place.
		orders [][]string
	}{
		{
			name: "equal zero shares",
			build: func() map[string]Device {
				return map[string]Device{"a-device": linuxDevice("a-device"), "b-device": linuxDevice("b-device")}
			},
			want:   "a-device",
			orders: [][]string{{"b-device", "a-device"}, {"a-device", "b-device"}},
		},
		{
			name: "equal non-zero shares",
			build: func() map[string]Device {
				a, b := linuxDevice("a-device"), linuxDevice("b-device")
				a.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 1}
				b.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 1}
				return map[string]Device{"a-device": a, "b-device": b}
			},
			want:   "a-device",
			orders: [][]string{{"b-device", "a-device"}, {"a-device", "b-device"}},
		},
		{
			name: "lower share outranks lower ID",
			build: func() map[string]Device {
				a, b, c := linuxDevice("a-device"), linuxDevice("b-device"), linuxDevice("c-device")
				a.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 2}
				b.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 1}
				c.Allocation = &ownsandv1.DeviceAllocation{VcpuCount: 1}
				return map[string]Device{"a-device": a, "b-device": b, "c-device": c}
			},
			want:   "b-device",
			orders: [][]string{{"c-device", "b-device", "a-device"}, {"a-device", "b-device", "c-device"}},
		},
	}
	for _, tc := range tests {
		for _, order := range tc.orders {
			t.Run(tc.name+" "+strings.Join(order, ","), func(t *testing.T) {
				byID := tc.build()
				devices := make([]Device, 0, len(order))
				for _, id := range order {
					devices = append(devices, byID[id])
				}
				dec, err := Place(linuxRequest(), devices, now)
				if err != nil || dec.DeviceID != tc.want {
					t.Fatalf("Place with input order %v chose %+v, %v; want device %q", order, dec, err, tc.want)
				}
			})
		}
	}
}
