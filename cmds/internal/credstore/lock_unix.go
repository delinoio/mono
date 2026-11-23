// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package credstore

import (
	"errors"
	"os"
	"syscall"
)

// tryLockFile takes an exclusive flock without blocking. It reports false
// when another open file description holds the lock.
func tryLockFile(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
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
