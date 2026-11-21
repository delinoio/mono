// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package guest

import (
	"errors"
	"os"
)

// reflink is not implemented on this guest OS (macOS clonefile and Windows
// block cloning need path-based or volume-specific calls); reads fall back
// to a checked full copy.
func reflink(dst, src *os.File) error {
	return errors.ErrUnsupported
}
