// SPDX-License-Identifier: Apache-2.0

package credstore

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"
)

// LockFileName is the name of the lock file that serializes access to the
// credential store across processes. It never holds a secret.
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
// releases it. The store directory must exist. The lock is
// an advisory OS file lock on a separate lock file, so the credential file
// can still be replaced atomically while the lock is held, and the OS
// releases the lock when the process exits.
func (s Store) lock() (func(), error) {
	return s.lockUntil(context.Background(), time.Now().Add(lockTimeout))
}

func (s Store) lockUntil(ctx context.Context, retryDeadline time.Time) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := openLockFile(filepath.Join(s.Dir, LockFileName))
	if err != nil {
		return nil, fmt.Errorf("credstore: open lock file: %w", err)
	}
	if limit := time.Now().Add(lockTimeout); limit.Before(retryDeadline) {
		retryDeadline = limit
	}
	for {
		ok, err := tryLockFile(f)
		if err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("credstore: lock: %w", err)
		}
		if ok {
			break
		}
		if !waitForRetry(ctx, retryDeadline, lockPollInterval) {
			_ = f.Close()
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return nil, ErrLockTimeout
		}
	}
	return func() {
		_ = unlockFile(f)
		_ = f.Close()
	}, nil
}

// waitForRetry reports whether another attempt can start after the delay.
func waitForRetry(ctx context.Context, deadline time.Time, delay time.Duration) bool {
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(min(remaining, delay))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil && time.Now().Before(deadline)
	}
}
