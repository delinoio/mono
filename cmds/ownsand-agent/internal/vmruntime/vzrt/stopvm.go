// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// This file holds the tag-free part of the VM stop path, so Linux CI can test
// it. The darwin runtime wires it to a Virtualization.framework VM.

// errStopTimeout reports that a VM did not reach a terminal state in time.
var errStopTimeout = errors.New("vzrt: the VM did not reach a terminal state in time")

// ErrVMMayRun marks a failure cleanup that could not confirm that the VM
// reached a terminal state. The VM may still use its files, so the caller must
// keep them (for example the staged disk of an image build).
var ErrVMMayRun = errors.New("vzrt: the VM may still run")

// StopTimeout bounds the stop request and terminal-state observation.
const StopTimeout = 10 * time.Second

// stopPollInterval is how often stopMachine re-reads the machine state while
// it waits, so a missing state-change notification cannot hide a stopped VM.
const stopPollInterval = 10 * time.Millisecond

// stoppableMachine is the part of a Virtualization.framework VM that the stop
// path uses.
type stoppableMachine interface {
	// Terminal reports whether the VM is stopped or failed.
	Terminal() bool
	// CanStop reports whether Stop may be called in the current state.
	CanStop() bool
	// Stop asks the framework to stop the VM. A nil result does not mean that
	// the VM is gone; the terminal state does.
	Stop() error
}

// machineStopper shares one pending framework request across stop attempts for
// a single VM. The framework calls cannot be canceled when a caller times out.
type machineStopper struct {
	mu      sync.Mutex
	pending *stopRequest
}

type stopRequest struct {
	done chan struct{}
	err  error
}

func (s *machineStopper) request(m stoppableMachine, exited <-chan struct{}) *stopRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil {
		return s.pending
	}
	req := &stopRequest{done: make(chan struct{})}
	s.pending = req
	go func() {
		// CanStop can block on the framework's dispatch queue. Check for
		// termination again before issuing Stop if CanStop returns late.
		if m.CanStop() && !machineTerminated(m, exited) {
			req.err = m.Stop()
		}
		s.mu.Lock()
		s.pending = nil
		close(req.done)
		s.mu.Unlock()
	}()
	return req
}

func machineTerminated(m stoppableMachine, exited <-chan struct{}) bool {
	select {
	case <-exited:
		return true
	default:
		return m.Terminal()
	}
}

// stopMachine uses one timeout for the framework request and the wait for
// termination. A caller can retry after a timeout. It must keep the VM's
// handle and resources until stopMachine confirms a terminal state.
func (s *machineStopper) stopMachine(m stoppableMachine, exited <-chan struct{}, timeout time.Duration) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	if machineTerminated(m, exited) {
		return nil
	}
	req := s.request(m, exited)
	done := req.done
	poll := time.NewTicker(stopPollInterval)
	defer poll.Stop()
	for {
		select {
		case <-exited:
			return nil
		case <-poll.C:
			if m.Terminal() {
				return nil
			}
		case <-done:
			if machineTerminated(m, exited) {
				return nil
			}
			if req.err != nil {
				return fmt.Errorf("vzrt: stop VM: %w", req.err)
			}
			done = nil
		case <-deadline.C:
			if machineTerminated(m, exited) {
				return nil
			}
			return errStopTimeout
		}
	}
}

// stopForCleanup stops m after the operation that owns it failed with primary
// and returns the error that the operation reports. It returns primary when m
// is in a terminal state afterwards. Otherwise it joins primary with the stop
// error, which wraps ErrVMMayRun, so the caller sees both the original cause
// and the failed cleanup, and keeps the files that the VM may still use. It
// uses timeout for both the framework request and the wait for a terminal
// state.
func stopForCleanup(primary error, m stoppableMachine, exited <-chan struct{}, timeout time.Duration) error {
	var stopper machineStopper
	if err := stopper.stopMachine(m, exited, timeout); err != nil {
		return errors.Join(primary, fmt.Errorf("%w: %w", ErrVMMayRun, err))
	}
	return primary
}

// releaseAfterStop ends the tracked VM v of id. It calls stop first and
// returns its error without any other change while the VM may still run:
// v stays in tracked, so a retry of the same operation finds the handle. Only
// after stop succeeds does it untrack v and call release, which removes the
// files. The caller serializes concurrent calls for one VM.
func releaseAfterStop[V comparable](mu *sync.Mutex, tracked map[string]V, id string, v V, stop func() error, release func() error) error {
	if err := stop(); err != nil {
		return err
	}
	mu.Lock()
	if tracked[id] == v {
		delete(tracked, id)
	}
	mu.Unlock()
	return release()
}
