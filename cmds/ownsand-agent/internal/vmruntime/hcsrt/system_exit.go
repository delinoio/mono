// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	stopTimeout       = 30 * time.Second
	exitRetryInterval = time.Second
)

var errSystemHandleClosed = errors.New("hcsrt: the compute system handle closed before exit was confirmed")

// systemExit tracks confirmation independently of the original HCS handle.
type systemExit struct {
	id           string
	exited       chan struct{}
	disconnected chan struct{}
	closed       chan struct{}
	exitOnce     sync.Once
	verifyExitFn func(string, time.Time) (bool, error)
}

func newSystemExit(id string, verify func(string, time.Time) (bool, error)) systemExit {
	return systemExit{
		id: id, exited: make(chan struct{}), disconnected: make(chan struct{}),
		closed: make(chan struct{}), verifyExitFn: verify,
	}
}

func (s *systemExit) markExited() { s.exitOnce.Do(func() { close(s.exited) }) }

func (s *systemExit) exitConfirmed() bool {
	select {
	case <-s.exited:
		return true
	default:
		return false
	}
}

// waitExit waits for a confirmed exit. After a disconnect, it queries by ID
// until HCS confirms exit, the handle closes, or the caller stops waiting.
func (s *systemExit) waitExit(ctx context.Context) error {
	type exitResult struct {
		confirmed bool
		err       error
	}
	disconnected := s.disconnected
	var result <-chan exitResult
	var retry *time.Timer
	var retryReady <-chan time.Time
	var statusErr error
	for !s.exitConfirmed() {
		select {
		case <-s.exited:
			return nil
		case <-s.closed:
			// Closing the handle can deliver a final exit callback.
			if s.exitConfirmed() {
				return nil
			}
			return errSystemHandleClosed
		case <-ctx.Done():
			if s.exitConfirmed() {
				return nil
			}
			return errors.Join(ctx.Err(), statusErr)
		case <-disconnected:
			disconnected = nil
		case <-retryReady:
			retryReady = nil
		case status := <-result:
			result = nil
			if status.confirmed {
				s.markExited()
				return nil
			}
			statusErr = status.err
			if retry == nil {
				retry = time.NewTimer(exitRetryInterval)
				defer retry.Stop()
			} else {
				retry.Reset(exitRetryInterval)
			}
			retryReady = retry.C
			continue
		}

		// A query trigger and a terminal signal can be ready together.
		if ctx.Err() != nil || s.exitConfirmed() {
			continue
		}
		select {
		case <-s.closed:
			continue
		default:
		}

		id, verify := s.id, s.verifyExitFn
		deadline := time.Now().Add(stopTimeout)
		if end, ok := ctx.Deadline(); ok && end.Before(deadline) {
			deadline = end
		}
		// The HCS call can finish after this waiter. It holds no reference
		// to the VM or its original handle.
		done := make(chan exitResult, 1)
		result = done
		go func() {
			confirmed, err := verify(id, deadline)
			done <- exitResult{confirmed, err}
		}()
	}
	return nil
}
