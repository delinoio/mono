// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// ErrLockTimeout reports that a contended unix socket path lock could not
// be acquired before the deadline. The message omits the path.
var ErrLockTimeout = errors.New("guestlisten: wait for the unix socket path lock timed out")

// ErrLockFailed reports a failure to open or take the lock of a unix socket
// path. The message names the operating system cause but omits the path.
var ErrLockFailed = errors.New("guestlisten: lock unix socket path failed")

// unixLockSuffix names the sidecar file that serializes the listener starts
// of one unix socket path. The file is never deleted: removing it while
// another start holds a lock on it would let a third start lock a new file
// and run beside the holder.
const unixLockSuffix = ".lock"

const (
	// unixLockPollInterval is the pause between two attempts to take the lock.
	unixLockPollInterval = 10 * time.Millisecond
	// unixLockWaitBudget bounds the elapsed time to acquire a contended lock.
	unixLockWaitBudget = 5 * time.Second
)

// Test seams for the lock wait. Production code never changes them.
var (
	unixLockNow = time.Now
	unixLockTry = tryLockFile
	// unixLockSleep pauses between two attempts to take the lock.
	unixLockSleep = time.Sleep
	// unixLockBudget is the wait budget of one lock wait.
	unixLockBudget = unixLockWaitBudget
	// beforeUnixRemove runs while the lock is held, after the probe found a
	// stale socket file and before the listener removes it. It lets a test
	// start a second listener inside the window that the lock closes.
	beforeUnixRemove func()
)

// lockUnixAddress serializes the listener starts of one unix socket path
// across goroutines and processes, and returns the function that releases the
// lock. The caller holds the lock from before it inspects the path until its
// bind has finished, so no other start probes, removes or binds the path in
// between. The lock is advisory and lives on the sidecar file "<path>.lock".
//
// A Linux abstract address has no filesystem entry and needs no lock, and a
// path in a missing directory cannot hold a lock file: in both cases the
// returned release is a no-op, and the bind reports its own path-free error.
// The returned errors never contain the path, because the caller logs them.
func lockUnixAddress(address string) (func(), error) {
	noop := func() {}
	if runtime.GOOS == "linux" && strings.HasPrefix(address, "@") {
		return noop, nil
	}
	deadline := unixLockNow().Add(unixLockBudget)
	retried := false
	wait := func() bool {
		retried = true
		remaining := deadline.Sub(unixLockNow())
		if remaining <= 0 {
			return false
		}
		unixLockSleep(min(unixLockPollInterval, remaining))
		return unixLockNow().Before(deadline)
	}
	f, err := openUnixLockFile(address+unixLockSuffix, wait)
	if errors.Is(err, os.ErrNotExist) {
		return noop, nil
	}
	if err != nil {
		return nil, pathFreeError(ErrLockFailed, err)
	}
	// The first immediate attempt is allowed. If opening the sidecar needed
	// a retry, the same deadline already applies to taking the lock.
	for !retried || unixLockNow().Before(deadline) {
		locked, err := unixLockTry(f)
		if err != nil {
			_ = f.Close()
			return nil, pathFreeError(ErrLockFailed, err)
		}
		if locked {
			if retried && !unixLockNow().Before(deadline) {
				unlockFile(f)
				break
			}
			return func() {
				unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if !wait() {
			break
		}
	}
	_ = f.Close()
	return nil, ErrLockTimeout
}

func openUnixLockFile(path string, wait func() bool) (*os.File, error) {
	const mode = 0o600
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return os.OpenFile(path, os.O_RDWR|os.O_CREATE, mode)
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, mode)
	if errors.Is(err, os.ErrExist) {
		for {
			existing, openErr := os.OpenFile(path, os.O_RDWR, 0)
			if errors.Is(openErr, os.ErrNotExist) {
				// A missing target, such as a dangling symlink, must not
				// let the listener proceed without a lock.
				return nil, err
			}
			// The creator may still be restoring permissions. Use the
			// same budget as flock and keep the open error on expiry.
			if !errors.Is(openErr, syscall.EACCES) || !wait() {
				return existing, openErr
			}
		}
	}
	if err != nil {
		return nil, err
	}
	// Only the creator changes the mode, on the same inode it will lock.
	if err := f.Chmod(mode); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
