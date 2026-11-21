// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux

package guest

import "os"

// isProcfs is always false: procfs exists only on Linux guests.
func isProcfs(f *os.File) bool {
	return false
}
