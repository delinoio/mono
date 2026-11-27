// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes a flock without blocking. It reports false
// when another open file description holds the lock.
func tryLockFile(f *os.File, mode enrollmentLockAccess) (bool, error) {
	flags := syscall.LOCK_SH | syscall.LOCK_NB
	if mode == enrollmentExclusive {
		flags = syscall.LOCK_EX | syscall.LOCK_NB
	}
	for {
		err := syscall.Flock(int(f.Fd()), flags)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EINTR):
			continue
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		default:
			return false, err
		}
	}
}

func unlockFile(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
