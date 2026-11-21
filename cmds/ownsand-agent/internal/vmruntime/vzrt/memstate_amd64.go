// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo && !arm64

package vzrt

import (
	"github.com/Code-Hex/vz/v3"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// memoryStateSupported: the binding saves machine state on Apple silicon
// only, so Intel Macs take disk-only snapshots.
const memoryStateSupported = false

func saveMemory(*vz.VirtualMachine, string) error { return vmruntime.ErrUnsupported }

func restoreMemory(*vz.VirtualMachine, string) error { return vmruntime.ErrUnsupported }
