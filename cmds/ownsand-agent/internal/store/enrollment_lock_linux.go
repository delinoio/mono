// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func chmodEnrollmentLock(parent int, before *unix.Stat_t) error {
	// O_PATH pins the lock inode even when its mode is 0000.
	fd, err := unix.Openat(parent, EnrollmentLockFile, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := verifyEnrollmentLockInode(fd, before); err != nil {
		return err
	}
	if err := unix.Fchmodat(fd, "", enrollmentLockMode, unix.AT_EMPTY_PATH); !errors.Is(err, unix.EOPNOTSUPP) && !errors.Is(err, unix.EINVAL) {
		return err
	}
	// Kernels without fchmodat2 or AT_EMPTY_PATH can reach the same pinned
	// inode through procfs. Remove this fallback once those kernels are out of use.
	return os.Chmod("/proc/self/fd/"+strconv.Itoa(fd), enrollmentLockMode)
}
