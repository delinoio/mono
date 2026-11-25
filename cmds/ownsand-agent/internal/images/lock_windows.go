// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package images

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLockFile reports false if another handle holds a conflicting byte lock.
func tryLockFile(f *os.File, mode lockMode) (bool, error) {
	flags := uint32(windows.LOCKFILE_FAIL_IMMEDIATELY)
	if mode == exclusiveLock {
		flags |= windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		flags, 0, 1, 0, ol)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION):
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(f *os.File) error {
	return windows.UnlockFileEx(windows.Handle(f.Fd()), 0, 1, 0, new(windows.Overlapped))
}
