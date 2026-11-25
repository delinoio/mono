// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"errors"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func chmodUnreadableLock(dirFD int, name string, expected *unix.Stat_t) error {
	fd, err := unix.Openat(dirFD, name, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return err
	}
	if stat.Dev != expected.Dev || stat.Ino != expected.Ino || !ownedRegularLock(&stat) {
		return unix.EACCES
	}
	err = unix.Fchmodat(fd, "", lockFileMode, unix.AT_EMPTY_PATH)
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.EINVAL) {
		// Older Linux kernels lack fchmodat2 or AT_EMPTY_PATH. The procfs
		// descriptor link still refers to the inode we opened.
		return os.Chmod("/proc/self/fd/"+strconv.Itoa(fd), lockFileMode)
	}
	return err
}
