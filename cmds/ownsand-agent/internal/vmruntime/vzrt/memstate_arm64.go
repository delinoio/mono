// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo && arm64

package vzrt

import "github.com/Code-Hex/vz/v3"

// memoryStateSupported: Virtualization.framework saves and restores the
// machine state of Apple silicon VMs (macOS 14 and later).
const memoryStateSupported = true

// saveMemory writes the state of a paused VM to path.
func saveMemory(m *vz.VirtualMachine, path string) error {
	return m.SaveMachineStateToPath(path)
}

// restoreMemory restores a stopped VM from path; the VM is paused after it.
func restoreMemory(m *vz.VirtualMachine, path string) error {
	return m.RestoreMachineStateFromURL(path)
}
