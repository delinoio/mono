// SPDX-License-Identifier: Apache-2.0

package imagebuild

import (
	"context"
	"fmt"
)

// buildWindows builds the Windows base image from opts.ISO.
func buildWindows(context.Context, string, Options) error {
	return fmt.Errorf("%w: the Windows image build is not implemented yet", ErrUnsupported)
}
