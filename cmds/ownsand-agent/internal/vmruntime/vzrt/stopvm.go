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

// stopMachine stops m and returns nil only when m is in a terminal state: the
// state is terminal, exited is closed, or the state became terminal while it
// waited. It returns an error when the framework rejects the stop or when the
// VM does not reach a terminal state within timeout; the VM may still run
// then, so the caller must keep its handle and may call stopMachine again.
func stopMachine(m stoppableMachine, exited <-chan struct{}, timeout time.Duration) error {
	if m.Terminal() {
		return nil
	}
	if m.CanStop() {
		if err := m.Stop(); err != nil {
			if m.Terminal() {
				// The VM ended on its own while the stop was rejected.
				return nil
			}
			return fmt.Errorf("vzrt: stop VM: %w", err)
		}
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
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
		case <-deadline.C:
			if m.Terminal() {
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
// waits at most timeout for the terminal state and never waits for a
// notification without a deadline.
func stopForCleanup(primary error, m stoppableMachine, exited <-chan struct{}, timeout time.Duration) error {
	if err := stopMachine(m, exited, timeout); err != nil {
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
