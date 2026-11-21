// SPDX-License-Identifier: Apache-2.0

package guest

import (
	"os"

	"golang.org/x/sys/unix"
)

// isProcfs reports whether f is open on a procfs file system. Regular files
// there report st_size 0 although a read returns bytes, so the byte count of
// a copy cannot be compared with the stat size.
func isProcfs(f *os.File) bool {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return false
	}
	return int64(st.Type) == unix.PROC_SUPER_MAGIC
}
