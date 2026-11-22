// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import "os"

// statSource returns the file information of the file at path, following a
// symlink. On these platforms os.Stat reads the identity (device and inode),
// size and modification time in one stat call.
func statSource(path string) (os.FileInfo, error) {
	return os.Stat(path)
}
