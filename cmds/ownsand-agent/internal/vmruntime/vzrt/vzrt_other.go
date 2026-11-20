// SPDX-License-Identifier: Apache-2.0

//go:build !darwin

// Package vzrt is the macOS runtime (Virtualization.framework).
package vzrt

import (
	"fmt"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
)

// New fails: Virtualization.framework exists on macOS only.
func New() (vmruntime.Runtime, error) {
	return nil, fmt.Errorf("%w: the Virtualization.framework runtime needs a macOS host", vmruntime.ErrUnsupported)
}
