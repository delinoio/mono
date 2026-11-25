// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package fsutil

import (
	"io/fs"
	"os"
)

func chmodNewTarDir(root *os.Root, name string, mode fs.FileMode) error {
	return root.Chmod(name, mode)
}
