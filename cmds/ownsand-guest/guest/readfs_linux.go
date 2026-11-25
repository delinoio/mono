// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"os"

	"golang.org/x/sys/unix"
)

// allowsSnapshotSizeMismatch checks the open descriptor because the source
// path can refer to another filesystem after the copy. Sysfs attributes can
// report a fixed size for generated contents, and procfs files can report zero
// even when a read returns bytes.
func allowsSnapshotSizeMismatch(f *os.File, statSize int64) bool {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return false
	}
	fsType := int64(st.Type)
	return fsType == unix.SYSFS_MAGIC || (fsType == unix.PROC_SUPER_MAGIC && statSize == 0)
}
