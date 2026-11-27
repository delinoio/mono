// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows"
)

const (
	statusLockFile = "status.lock"
	statusWait     = time.Second
)

func writeStatusFile(ctx context.Context, path string, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeFileAtomic(path, data, 0o600, func(src, dst string) error {
		ctx, cancel := context.WithTimeout(ctx, statusWait)
		defer cancel()
		lock, err := openStatusLock(ctx, dst, windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
		if err != nil {
			return err
		}
		defer windows.CloseHandle(lock)
		if err := lockStatus(ctx, lock, windows.LOCKFILE_EXCLUSIVE_LOCK); err != nil {
			return err
		}
		defer unlockStatus(lock)
		from, err := statusWindowsPath(src)
		if err != nil {
			return err
		}
		to, err := statusWindowsPath(dst)
		if err != nil {
			return err
		}
		// The temporary file is complete and synced. Never take the old
		// destination away before publishing the new one.
		return retryStatus(ctx, isSharingError, func() error {
			if err := windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
				return &os.LinkError{Op: "MoveFileEx", Old: src, New: dst, Err: err}
			}
			return nil
		})
	})
}

func readStatusFile(path string) ([]byte, error) {
	return readWindowsStatus(path, os.ReadFile)
}

func readWindowsStatus(path string, read func(string) ([]byte, error)) ([]byte, error) {
	data, err := read(path)
	if !errors.Is(err, fs.ErrNotExist) && !isSharingError(err) {
		return data, err
	}
	// Windows can return FILE_NOT_FOUND while a replacement is in progress.
	// The writer holds the exclusive lock across the move. An ordinary read
	// needs no lock, but a failed open must recheck under a shared lock before
	// reporting absence. Keep this workaround while Windows can expose the
	// rename gap. Readers never create the lock file.
	ctx, cancel := context.WithTimeout(context.Background(), statusWait)
	defer cancel()
	lock, lockErr := openStatusLock(ctx, path, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if errors.Is(lockErr, fs.ErrNotExist) {
		return data, err
	}
	if lockErr != nil {
		return nil, lockErr
	}
	defer windows.CloseHandle(lock)
	if err := lockStatus(ctx, lock, 0); err != nil {
		return nil, err
	}
	defer unlockStatus(lock)
	err = retryStatus(ctx, isSharingError, func() error {
		data, err = read(path)
		return err
	})
	return data, err
}

func openStatusLock(ctx context.Context, path string, access, disposition uint32) (windows.Handle, error) {
	path = filepath.Join(filepath.Dir(path), statusLockFile)
	name, err := statusWindowsPath(path)
	if err != nil {
		return windows.InvalidHandle, err
	}
	// Reset can delete this file even while a status reader holds it.
	var handle windows.Handle
	err = retryStatus(ctx, isSharingError, func() error {
		handle, err = windows.CreateFile(name, access,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			nil, disposition, windows.FILE_ATTRIBUTE_NORMAL, 0)
		return err
	})
	if err != nil {
		return windows.InvalidHandle, &os.PathError{Op: "open status lock", Path: path, Err: err}
	}
	return handle, nil
}

func lockStatus(ctx context.Context, handle windows.Handle, flags uint32) error {
	isBusy := func(err error) bool { return errors.Is(err, windows.ERROR_LOCK_VIOLATION) }
	err := retryStatus(ctx, isBusy, func() error {
		return windows.LockFileEx(handle, flags|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	})
	if err != nil {
		return fmt.Errorf("store: lock status publication: %w", err)
	}
	return nil
}

func unlockStatus(handle windows.Handle) {
	_ = windows.UnlockFileEx(handle, 0, 1, 0, new(windows.Overlapped))
}

func retryStatus(ctx context.Context, retry func(error) bool, operation func() error) error {
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(lastErr, err)
		}
		lastErr = operation()
		if !retry(lastErr) {
			return lastErr
		}
		timer := time.NewTimer(replaceRetryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

// The Win32 calls need extended paths for long names, as os.Rename does.
func statusWindowsPath(path string) (*uint16, error) {
	full, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	switch {
	case len(full) < 248, strings.HasPrefix(full, `\\?\`), strings.HasPrefix(full, `\\.\`), strings.HasPrefix(full, `\??\`):
	case strings.HasPrefix(full, `\\`):
		full = `\\?\UNC\` + full[2:]
	default:
		full = `\\?\` + full
	}
	return windows.UTF16PtrFromString(full)
}
