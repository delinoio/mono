// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"errors"
	"math"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

func TestMemoryMiBToBytes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		memory   int64
		bytes    uint64
		rejected bool
	}{
		{name: "one MiB", memory: 1, bytes: 1_048_576},
		{name: "typical", memory: 4096, bytes: 4_294_967_296},
		{name: "maximum", memory: math.MaxUint64 >> 20, bytes: math.MaxUint64 - 1_048_575},
		{name: "first overflow", memory: (math.MaxUint64 >> 20) + 1, rejected: true},
		{name: "zero", rejected: true},
		{name: "negative", memory: -1, rejected: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := memoryMiBToBytes(tc.memory)
			if tc.rejected {
				if got != 0 || !errors.Is(err, vmruntime.ErrUnsupported) {
					t.Fatalf("memoryMiBToBytes(%d) = %d, %v, want 0 and ErrUnsupported", tc.memory, got, err)
				}
				return
			}
			if err != nil || got != tc.bytes {
				t.Fatalf("memoryMiBToBytes(%d) = %d, %v, want %d bytes", tc.memory, got, err, tc.bytes)
			}
		})
	}
}
