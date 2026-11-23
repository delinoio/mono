// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import "os"

// replaceFile moves the finished temporary file over the destination. A
// rename replaces a destination that another process has open on this
// platform, so no retry is needed.
func replaceFile(oldPath, newPath string) error {
	return os.Rename(oldPath, newPath)
}
