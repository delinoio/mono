// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo && amd64

package vzrt

import (
	"errors"

	"github.com/Code-Hex/vz/v3"
)

// macOSGuestsSupported: Virtualization.framework runs macOS guests only on
// Apple silicon, not on Intel Macs.
const macOSGuestsSupported = false

// NewMacOSConfiguration fails on Intel Macs.
func NewMacOSConfiguration(MacOSMachine) (*vz.VirtualMachineConfiguration, []byte, error) {
	return nil, nil, errors.New("vzrt: macOS guests need an Apple silicon Mac")
}

func macOSHardwareModelSupported([]byte) bool { return false }
