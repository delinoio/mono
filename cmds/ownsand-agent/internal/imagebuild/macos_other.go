// SPDX-License-Identifier: Apache-2.0

//go:build !(darwin && cgo && arm64)

package imagebuild

import (
	"context"
	"fmt"
)

// buildMacOS needs Virtualization.framework (cgo) on an Apple silicon Mac.
func buildMacOS(context.Context, string, Options) error {
	return fmt.Errorf("%w: macOS images are built on an Apple silicon Mac by an agent built with cgo", ErrUnsupported)
}
