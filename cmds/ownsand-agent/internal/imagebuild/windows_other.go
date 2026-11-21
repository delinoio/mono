// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package imagebuild

import (
	"context"
	"fmt"
)

// buildWindows needs Hyper-V and the Windows image tools of a Windows host.
func buildWindows(context.Context, string, Options) error {
	return fmt.Errorf("%w: Windows images are built on a Windows host with Hyper-V", ErrUnsupported)
}
