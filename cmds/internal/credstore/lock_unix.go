// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package credstore

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"golang.org/x/sys/unix"
)

const lockFileMode = 0o600

func openLockFile(path string) (*os.File, error) {
	const flags = os.O_RDWR | os.O_CREATE | unix.O_NOFOLLOW | unix.O_NONBLOCK
	f, err := os.OpenFile(path, flags, lockFileMode)
	if errors.Is(err, unix.EACCES) {
		if err := repairLockPermissions(path, err); err != nil {
			return nil, err
		}
		f, err = os.OpenFile(path, flags, lockFileMode)
	}
	if err != nil {
		return nil, err
	}

	var stat unix.Stat_t
	err = unix.Fstat(int(f.Fd()), &stat)
	if err == nil {
		switch {
		case !ownedRegularLock(&stat):
			err = errors.New("lock must be a regular file owned by the current user")
		case stat.Mode&0o7777 != lockFileMode:
			// The umask can remove owner access when this process creates the file.
			err = f.Chmod(lockFileMode)
		}
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

func repairLockPermissions(path string, openErr error) error {
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return openErr
	}
	defer func() { _ = dir.Close() }()
	dirFD := int(dir.Fd())
	var stat unix.Stat_t
	if err := unix.Fstat(dirFD, &stat); err != nil ||
		stat.Mode&(unix.S_IFMT|0o7777) != unix.S_IFDIR|0o700 || int(stat.Uid) != os.Geteuid() {
		return openErr
	}
	name := filepath.Base(path)
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil || !ownedRegularLock(&stat) {
		return openErr
	}
	if stat.Mode&lockFileMode == lockFileMode {
		// Another process can restore owner access between the open and this stat.
		return nil
	}
	// Change the existing lock in place so a process that already holds it
	// continues to exclude every contender.
	if err := chmodUnreadableLock(dirFD, name, &stat); err != nil {
		return &os.PathError{Op: "chmod", Path: path, Err: err}
	}
	return nil
}

func ownedRegularLock(stat *unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && int(stat.Uid) == os.Geteuid()
}

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
