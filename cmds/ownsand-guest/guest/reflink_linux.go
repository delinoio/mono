//go:build linux

package guest

import (
	"os"
	"syscall"
)

// ficlone is the FICLONE ioctl request number (_IOW(0x94, 9, int)).
const ficlone = 0x40049409

// reflink makes dst share the extents of src (btrfs, XFS and other file
// systems with reflink support). It fails on other file systems and across
// file systems, and the caller then copies.
func reflink(dst, src *os.File) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, dst.Fd(), ficlone, src.Fd())
	if errno != 0 {
		return errno
	}
	return nil
}
