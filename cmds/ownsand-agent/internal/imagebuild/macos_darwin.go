// SPDX-License-Identifier: Apache-2.0

//go:build darwin && cgo && arm64

package imagebuild

import (
	"context"
	"fmt"
)

// buildMacOS builds the macOS base image from an Apple restore image.
func buildMacOS(context.Context, string, Options) error {
	return fmt.Errorf("%w: the macOS image build is not implemented yet", ErrUnsupported)
}
