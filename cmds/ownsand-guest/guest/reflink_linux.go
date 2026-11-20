// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"os"

	"golang.org/x/sys/unix"
)

// reflink clones src into dst with FICLONE where the file system supports
// it (for example btrfs and XFS). It fails on other file systems and across
// file systems.
func reflink(dst, src *os.File) error {
	return unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
}
