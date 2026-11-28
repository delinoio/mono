// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// EnrollmentLockFile coordinates enrollment, image builds and destructive
// cleanup in the state directory. The file is empty. The lock uses its first byte.
const EnrollmentLockFile = "enrollment.lock"

// enrollmentLockPoll is the interval at which a waiting LockEnrollment tries
// the lock again. The lock call itself never blocks, so that the wait ends
// with its context on every platform.
const enrollmentLockPoll = 25 * time.Millisecond

// ErrEnrollmentBusy reports that the enrollment lock could not be acquired.
var ErrEnrollmentBusy = errors.New("store: the enrollment lock is busy for this state directory")

// ErrRemovalPending reports that an authenticated removal notice blocks a build.
var ErrRemovalPending = errors.New("store: device removal is pending for this state directory")

type enrollmentLockAccess uint8

const (
	enrollmentExclusive enrollmentLockAccess = iota
	enrollmentShared
)

type enrollmentLockBusyObserverKey struct{}

// WithEnrollmentLockBusyObserver attaches a test observer to a lock acquisition.
// The observer runs once when the acquisition first finds the file lock busy.
// It runs inline and must return promptly.
func WithEnrollmentLockBusyObserver(ctx context.Context, observe func()) context.Context {
	return context.WithValue(ctx, enrollmentLockBusyObserverKey{}, observe)
}

// LockEnrollment takes the exclusive enrollment lock of the state directory
// and returns the function that releases it. The lock is an advisory file
// lock (flock, or LockFileEx on Windows), so it excludes other Store values
// and other processes that use the same directory, and the operating system
// releases it when the holder exits or crashes. It waits until another
// holder releases the lock or ctx ends. If ctx ends before the lock can
// be accepted, it returns ErrEnrollmentBusy together with the context error.
// Different state directories use different locks and never wait for each other.
func (s *Store) LockEnrollment(ctx context.Context) (unlock func(), err error) {
	return acquireEnrollmentLock(ctx, s.dir, enrollmentExclusive)
}

// LockImageBuild admits a builder before it creates the store layout. It tries
// the shared enrollment lock once. While holding it, the builder checks the
// removal notice and keeps the lock until all build cleanup has returned.
func (s *Store) LockImageBuild(ctx context.Context) (unlock func(), err error) {
	if err := enrollmentWaitError(ctx); err != nil {
		return nil, err
	}
	if err := PrepareDir(s.dir); err != nil {
		return nil, fmt.Errorf("store: prepare state directory for image build: %w", err)
	}
	unlock, err = acquireEnrollmentLock(ctx, s.dir, enrollmentShared)
	if err != nil {
		return nil, err
	}
	state, err := s.LoadAgent()
	if err == nil && state.Removal.NoticeReceived {
		err = ErrRemovalPending
	}
	if err != nil && !errors.Is(err, ErrNotFound) {
		unlock()
		return nil, err
	}
	return unlock, nil
}

func acquireEnrollmentLock(ctx context.Context, dir string, mode enrollmentLockAccess) (unlock func(), err error) {
	if err := enrollmentWaitError(ctx); err != nil {
		return nil, err
	}
	f, err := openEnrollmentLock(dir)
	if err != nil {
		return nil, fmt.Errorf("store: open enrollment lock: %w", err)
	}
	if err := waitEnrollmentLock(ctx, f, mode, tryLockFile); err != nil {
		_ = f.Close()
		return nil, err
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

func waitEnrollmentLock(ctx context.Context, f *os.File, mode enrollmentLockAccess, tryLock func(*os.File, enrollmentLockAccess) (bool, error)) error {
	observeBusy, _ := ctx.Value(enrollmentLockBusyObserverKey{}).(func())
	for {
		if err := enrollmentWaitError(ctx); err != nil {
			return err
		}
		ok, err := tryLock(f, mode)
		if err != nil {
			return fmt.Errorf("store: lock enrollment: %w", err)
		}
		if ok {
			if err := enrollmentWaitError(ctx); err != nil {
				_ = unlockFile(f)
				return err
			}
			return nil
		}
		if observeBusy != nil {
			observeBusy()
			observeBusy = nil
		}
		if mode == enrollmentShared {
			return ErrEnrollmentBusy
		}
		timer := time.NewTimer(enrollmentLockPoll)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func enrollmentWaitError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("%w: %w", ErrEnrollmentBusy, err)
	}
	// The clock can reach the deadline before the context timer runs.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return fmt.Errorf("%w: %w", ErrEnrollmentBusy, context.DeadlineExceeded)
	}
	return nil
}
