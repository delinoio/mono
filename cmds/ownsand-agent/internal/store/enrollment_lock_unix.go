// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package store

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

const (
	enrollmentLockMode      = 0o600
	enrollmentLockOpenFlags = unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
)

func openEnrollmentLock(dir string) (*os.File, error) {
	path := filepath.Join(dir, EnrollmentLockFile)
	fd, err := unix.Open(path, enrollmentLockOpenFlags|unix.O_RDWR|unix.O_CREAT, enrollmentLockMode)
	if errors.Is(err, unix.EACCES) {
		fd, err = reopenEnrollmentLock(dir)
	}
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	var info unix.Stat_t
	if err := unix.Fstat(fd, &info); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if err := validateEnrollmentLock(&info); err != nil {
		unix.Close(fd)
		return nil, err
	}
	if info.Mode&enrollmentLockMode != enrollmentLockMode {
		if err := unix.Fchmod(fd, enrollmentLockMode); err != nil {
			unix.Close(fd)
			return nil, err
		}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func validateEnrollmentLock(info *unix.Stat_t) error {
	if info.Mode&unix.S_IFMT != unix.S_IFREG || info.Nlink != 1 {
		return errors.New("enrollment lock must be a regular file with one link")
	}
	if info.Uid != uint32(os.Geteuid()) || info.Mode&0o7777&^enrollmentLockMode != 0 {
		return unix.EACCES
	}
	return nil
}

func reopenEnrollmentLock(stateDir string) (int, error) {
	parent, err := unix.Open(stateDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, err
	}
	defer unix.Close(parent)

	var dir, before unix.Stat_t
	if err := unix.Fstat(parent, &dir); err != nil {
		return -1, err
	}
	if dir.Uid != uint32(os.Geteuid()) || dir.Mode&0o777 != 0o700 {
		return -1, unix.EACCES
	}
	if err := unix.Fstatat(parent, EnrollmentLockFile, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return -1, err
	}
	if err := validateEnrollmentLock(&before); err != nil {
		return -1, err
	}
	// An older agent, or a concurrent creator that has yet to set the mode,
	// can leave the lock without owner read or write access. Repair the
	// existing file so callers that already opened it still share the lock.
	if before.Mode&enrollmentLockMode != enrollmentLockMode {
		if err := chmodEnrollmentLock(parent, &before); err != nil {
			return -1, err
		}
	}

	fd, err := unix.Openat(parent, EnrollmentLockFile, enrollmentLockOpenFlags|unix.O_RDWR, 0)
	if err != nil {
		return -1, err
	}
	if err := verifyEnrollmentLockInode(fd, &before); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func verifyEnrollmentLockInode(fd int, before *unix.Stat_t) error {
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		return err
	}
	if current.Dev != before.Dev || current.Ino != before.Ino {
		return errors.New("enrollment lock changed during permission repair")
	}
	return validateEnrollmentLock(&current)
}
