// SPDX-License-Identifier: FSL-1.1-ALv2

package vmruntime

import (
	"fmt"
	"os"
)

// CheckLinuxBaseDiskSize checks a raw Linux base disk's logical size, including
// sparse regions, against the requested capacity.
func CheckLinuxBaseDiskSize(path string, requestedGiB int64) error {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: the Linux base image is missing", ErrUnsupported)
	}
	const gib = 1 << 30
	size := info.Size()
	requiredGiB := size / gib
	if size%gib != 0 {
		requiredGiB++
	}
	if requestedGiB >= requiredGiB {
		return nil
	}
	return fmt.Errorf("%w: the Linux base image needs at least %d GiB of disk (requested %d GiB)",
		ErrUnsupported, requiredGiB, requestedGiB)
}
