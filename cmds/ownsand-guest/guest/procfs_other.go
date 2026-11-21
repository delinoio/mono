// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package guest

import "os"

// isProcfs is always false: procfs exists only on Linux guests.
func isProcfs(f *os.File) bool {
	return false
}
