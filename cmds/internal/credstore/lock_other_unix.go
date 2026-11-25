// SPDX-License-Identifier: Apache-2.0

//go:build !linux && !windows

package credstore

import "golang.org/x/sys/unix"

func chmodUnreadableLock(dirFD int, name string, _ *unix.Stat_t) error {
	return unix.Fchmodat(dirFD, name, lockFileMode, unix.AT_SYMLINK_NOFOLLOW)
}
