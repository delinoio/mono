// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package store

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func openEnrollmentLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, EnrollmentLockFile)
	name, err := statePathName(path)
	if err != nil {
		return nil, err
	}
	// Omit delete sharing so concurrent callers keep locking the same file.
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_ALWAYS,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info windows.ByHandleFileInformation
	err = windows.GetFileInformationByHandle(h, &info)
	if err == nil {
		switch {
		case info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0:
			err = &StatePathError{Path: path, Reason: StatePathReparsePoint}
		case info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0:
			err = &StatePathError{Path: path, Reason: StatePathNotRegularFile}
		case info.NumberOfLinks != 1:
			err = &StatePathError{Path: path, Reason: StatePathMultipleLinks}
		}
	}
	if err != nil {
		windows.CloseHandle(h)
		return nil, err
	}
	return os.NewFile(uintptr(h), path), nil
}

// tryLockFile takes an exclusive LockFileEx lock on the first byte without
// waiting. It reports false when another handle holds the lock.
func tryLockFile(f *os.File) (bool, error) {
	ol := new(windows.Overlapped)
	err := windows.LockFileEx(windows.Handle(f.Fd()),
		windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
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
