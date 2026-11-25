// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !linux && !windows

package store

import "golang.org/x/sys/unix"

func chmodEnrollmentLock(parent int, before *unix.Stat_t) error {
	var access int
	switch {
	case before.Mode&0o400 != 0:
		access = unix.O_RDONLY
	case before.Mode&0o200 != 0:
		access = unix.O_WRONLY
	default:
		// With mode 0000, an ordinary open cannot give us a descriptor.
		// Use the pinned private directory and never follow the final link.
		// The caller checks the inode again when it reopens the file.
		return unix.Fchmodat(parent, EnrollmentLockFile, enrollmentLockMode, unix.AT_SYMLINK_NOFOLLOW)
	}
	fd, err := unix.Openat(parent, EnrollmentLockFile, enrollmentLockOpenFlags|access, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := verifyEnrollmentLockInode(fd, before); err != nil {
		return err
	}
	return unix.Fchmod(fd, enrollmentLockMode)
}
