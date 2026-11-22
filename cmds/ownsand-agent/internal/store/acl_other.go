// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import "os"

// restrictStateDir limits the state directory to its owner. The directory
// may exist with wider permissions from an operator.
func restrictStateDir(dir string) error {
	return os.Chmod(dir, 0o700)
}
