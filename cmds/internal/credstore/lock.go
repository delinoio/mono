// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LockFileName is the name of the lock file that serializes the changes of
// the credential file across processes. It never holds a secret.
const LockFileName = "credentials.lock"

// lockTimeout bounds the wait for the store lock. Holders only run local file
// operations, so a longer wait means that a holder is stuck.
const lockTimeout = 30 * time.Second

// lockPollInterval is the pause between attempts to take a held lock.
const lockPollInterval = 5 * time.Millisecond

// ErrLockTimeout reports that another process held the credential store lock
// for longer than the lock timeout.
var ErrLockTimeout = errors.New("credstore: timed out waiting for the credential store lock")

// lock takes the cross-process store lock and returns a function that
// releases it. The caller must have created the store directory. The lock is
// an advisory OS file lock on a separate lock file, so the credential file
// can still be replaced atomically while the lock is held, and the OS
// releases the lock when the process exits.
func (s Store) lock() (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.Dir, LockFileName), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("credstore: open lock file: %w", err)
	}
	deadline := time.Now().Add(lockTimeout)
	for {
		ok, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("credstore: lock: %w", err)
		}
		if ok {
			break
		}
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, ErrLockTimeout
		}
		time.Sleep(lockPollInterval)
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}
