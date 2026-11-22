// SPDX-License-Identifier: FSL-1.1-ALv2

package images

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrLocked reports that another handle holds the lock of an image
// destination. A build holds it from before it removes the staging directory
// until its install and cleanup are done.
var ErrLocked = errors.New("images: the image destination is locked by another process")

// Lock is an exclusive cross-process lock on an image destination. The
// operating system releases it when the holder closes the file or ends for
// any reason, so a crashed build never blocks a later one.
type Lock struct {
	f *os.File
}

// LockPath returns the lock file of an image directory: the sibling
// `<dir>.build.lock` (for example images/windows.build.lock), next to the
// staging directory `<dir>.build` that it protects. Install, Recover and the
// platform builders never remove it, so every process locks the same file.
func LockPath(dir string) string { return dir + ".build.lock" }

// TryLock takes the exclusive lock of the image directory dir without
// waiting. It returns ErrLocked when another handle holds the lock. The
// image build holds the lock for its whole run; Recover takes it so that it
// never exchanges directories under a running Install.
func TryLock(dir string) (*Lock, error) {
	path := LockPath(dir)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create the images directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the image lock: %w", err)
	}
	ok, err := tryLockFile(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("lock %s: %w", path, err)
	}
	if !ok {
		_ = f.Close()
		return nil, ErrLocked
	}
	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file. It leaves the file in place:
// removing it would let a waiting process lock an unlinked file while a third
// process creates and locks a new one.
func (l *Lock) Release() error {
	unlockErr := unlockFile(l.f)
	closeErr := l.f.Close()
	return errors.Join(unlockErr, closeErr)
}
