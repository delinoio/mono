// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestVirtualizationRejectsMemoryOverflow(t *testing.T) {
	const overflowMiB = (math.MaxUint64 >> 20) + 1

	// The fixtures use a missing kernel and an invalid hardware model.
	// Memory validation must run before either configuration uses them.
	t.Run("Linux guest", func(t *testing.T) {
		spec := vmruntime.Spec{Resources: &ownsandv1.Resources{VcpuCount: 2, MemoryMib: overflowMiB}}
		cfg, err := (&Runtime{}).configure(&vm{}, spec, nil)
		if cfg != nil || !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "VM memory") {
			t.Fatalf("Linux configuration = %v, %v, want a memory resource error", cfg, err)
		}
	})
	t.Run("macOS guest", func(t *testing.T) {
		if !macOSGuestsSupported {
			t.Skip("macOS guests need an Apple silicon Mac")
		}
		machine := MacOSMachine{CPUs: 2, MemoryMiB: overflowMiB, HardwareModel: []byte{0}}
		cfg, id, err := NewMacOSConfiguration(machine)
		if cfg != nil || id != nil || !errors.Is(err, vmruntime.ErrUnsupported) || !strings.Contains(err.Error(), "VM memory") {
			t.Fatalf("macOS configuration = %v, %v, %v, want a memory resource error", cfg, id, err)
		}
	})
}
