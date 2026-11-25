// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// EnrollmentLockFile is the lock file of the enrollment sequence inside the
// state directory. It holds no data; the lock is on its first byte.
const EnrollmentLockFile = "enrollment.lock"

// enrollmentLockPoll is the interval at which a waiting LockEnrollment tries
// the lock again. The lock call itself never blocks, so that the wait ends
// with its context on every platform.
const enrollmentLockPoll = 25 * time.Millisecond

// ErrEnrollmentBusy reports that another enrollment holds the enrollment
// lock of the state directory and did not release it before the wait ended.
var ErrEnrollmentBusy = errors.New("store: another enrollment is running for this state directory")

// LockEnrollment takes the exclusive enrollment lock of the state directory
// and returns the function that releases it. The lock is an advisory file
// lock (flock, or LockFileEx on Windows), so it excludes other Store values
// and other processes that use the same directory, and the operating system
// releases it when the holder exits or crashes. It waits until another
// holder releases the lock or ctx ends; then it returns ErrEnrollmentBusy
// together with the error of ctx. Different state directories use different
// locks and never wait for each other.
//
// Reset removes the lock file with every other entry. On Windows an open
// locked file cannot be removed, so a Reset that runs while an enrollment
// holds the lock reports an error for the lock file.
func (s *Store) LockEnrollment(ctx context.Context) (unlock func(), err error) {
	f, err := openEnrollmentLock(s.dir)
	if err != nil {
		return nil, fmt.Errorf("store: open enrollment lock: %w", err)
	}
	for {
		ok, lerr := tryLockFile(f)
		if lerr != nil {
			f.Close()
			return nil, fmt.Errorf("store: lock enrollment: %w", lerr)
		}
		if ok {
			break
		}
		timer := time.NewTimer(enrollmentLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			f.Close()
			return nil, fmt.Errorf("%w: %w", ErrEnrollmentBusy, ctx.Err())
		case <-timer.C:
		}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			// Closing the file releases the lock too; the explicit unlock
			// keeps the release independent of when the descriptor closes.
			_ = unlockFile(f)
			_ = f.Close()
		})
	}, nil
}
