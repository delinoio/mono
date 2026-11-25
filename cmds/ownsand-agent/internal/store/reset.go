// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const resetEnrollmentLockWait = 45 * time.Second

// ResetDir deletes the state directory contents, including the identity,
// while holding the enrollment lock. The directory and lock file stay.
// ResetDir needs no Store and works when the runtime layout is damaged.
//
// An absent directory counts as empty. If the lock cannot be acquired, no
// entries are removed. Once the lock is held, ResetDir attempts every
// deletion and returns all failures together.
func ResetDir(dir string) error {
	ctx, cancel := context.WithTimeout(context.Background(), resetEnrollmentLockWait)
	defer cancel()
	return resetDir(ctx, dir)
}

func resetDir(ctx context.Context, dir string) error {
	if dir == "" {
		return errors.New("store: state directory is required")
	}
	// Stat before ReadDir: on Windows, reading a regular file as a directory
	// fails with ERROR_PATH_NOT_FOUND, which Go maps to os.ErrNotExist, so the
	// ReadDir error alone cannot tell an absent directory from a file.
	info, err := os.Stat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store: stat state directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("store: state directory %q is not a directory", dir)
	}
	unlock, err := acquireEnrollmentLock(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("store: read state directory: %w", err)
	}
	var errs []error
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		// A process waiting on the open file must still share this lock
		// with processes that open it after reset.
		if e.Name() == EnrollmentLockFile {
			continue
		}
		if strings.EqualFold(e.Name(), EnrollmentLockFile) {
			// The lock can have different casing on a case-insensitive volume.
			lock, lockErr := os.Lstat(filepath.Join(dir, EnrollmentLockFile))
			entry, entryErr := e.Info()
			if err := errors.Join(lockErr, entryErr); err != nil {
				errs = append(errs, err)
				continue
			}
			if os.SameFile(lock, entry) {
				continue
			}
		}
		if err := removeAll(path); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
