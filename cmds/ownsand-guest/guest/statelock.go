// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

// ErrStateDirInUse reports a state directory that another helper process
// holds. The message omits the path.
var ErrStateDirInUse = errors.New("guest: state directory is in use by another helper")

// stateLockName is the lock file in the state directory. The helper never
// removes it: the lock belongs to the open file, not to the name.
const stateLockName = "guest.lock"

// stateLock is the exclusive ownership of a state directory. The operating
// system releases it when the process ends, so a helper that crashed never
// leaves a stale lock and its replacement reclaims the directory.
type stateLock struct {
	once sync.Once
	file *os.File
}

// lockStateDir takes the exclusive lock of dir without waiting. It creates
// dir when it is missing. It returns ErrStateDirInUse when another open file
// description or process holds the lock, which includes a second helper of
// the same process. The returned errors omit the path, because the caller
// logs them.
func lockStateDir(dir string) (*stateLock, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("guest: create state directory: %w", pathFree(err))
	}
	f, err := os.OpenFile(filepath.Join(dir, stateLockName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("guest: open state directory lock: %w", pathFree(err))
	}
	locked, err := tryLockFile(f)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("guest: lock state directory: %w", pathFree(err))
	}
	if !locked {
		_ = f.Close()
		return nil, ErrStateDirInUse
	}
	return &stateLock{file: f}, nil
}

// release drops the lock. It is safe to call more than once. Closing the
// file releases the lock on every platform.
func (l *stateLock) release() {
	if l == nil {
		return
	}
	l.once.Do(func() { _ = l.file.Close() })
}

// pathFree returns the operating system error inside a *os.PathError, whose
// text repeats the path.
func pathFree(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
