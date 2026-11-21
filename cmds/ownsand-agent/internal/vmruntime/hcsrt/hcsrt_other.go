// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package hcsrt

import (
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// New fails: the Host Compute Service exists on Windows only.
func New(Options) (vmruntime.Runtime, error) {
	return nil, fmt.Errorf("%w: the Hyper-V runtime needs a Windows host", vmruntime.ErrUnsupported)
}
