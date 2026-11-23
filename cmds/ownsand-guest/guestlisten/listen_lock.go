// SPDX-License-Identifier: FSL-1.1-ALv2

package guestlisten

import (
	"errors"
	"os"
	"runtime"
	"strings"
	"time"
)

// ErrLockTimeout reports that another listener start held the lock of the
// same unix socket path for the whole wait budget. The message omits the
// path.
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
	// unixLockWaitBudget bounds the total pause of one lock wait. The wait
	// counts its pauses instead of reading a clock, so the bound does not
	// depend on timer granularity.
	unixLockWaitBudget = 5 * time.Second
)

// Test seams for the lock wait. Production code never changes them.
var (
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
	f, err := os.OpenFile(address+unixLockSuffix, os.O_RDWR|os.O_CREATE, 0o600)
	if errors.Is(err, os.ErrNotExist) {
		return noop, nil
	}
	if err != nil {
		return nil, pathFreeError(ErrLockFailed, err)
	}
	for waited := time.Duration(0); ; waited += unixLockPollInterval {
		locked, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, pathFreeError(ErrLockFailed, err)
		}
		if locked {
			return func() {
				unlockFile(f)
				_ = f.Close()
			}, nil
		}
		if waited >= unixLockBudget {
			_ = f.Close()
			return nil, ErrLockTimeout
		}
		unixLockSleep(unixLockPollInterval)
	}
}
