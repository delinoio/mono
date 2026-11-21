// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !darwin

package vzrt

import (
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// New fails: Virtualization.framework exists on macOS only.
func New(Options) (vmruntime.Runtime, error) {
	return nil, fmt.Errorf("%w: the Virtualization.framework runtime needs a macOS host", vmruntime.ErrUnsupported)
}
