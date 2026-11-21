// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package fcrt

import (
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// NewRuntime fails: Firecracker needs a Linux host with KVM.
func NewRuntime(Options) (vmruntime.Runtime, error) {
	return nil, fmt.Errorf("%w: the Firecracker runtime needs a Linux host with KVM", vmruntime.ErrUnsupported)
}
