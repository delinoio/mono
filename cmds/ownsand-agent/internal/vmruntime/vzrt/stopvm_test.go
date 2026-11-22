// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeMachine is a scripted stoppableMachine.
type fakeMachine struct {
	mu       sync.Mutex
	terminal bool
	canStop  bool
	stopErr  error
	// stopEnds makes a successful Stop move the machine to a terminal state.
	stopEnds bool
	stops    int
}

func (f *fakeMachine) Terminal() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.terminal
}

func (f *fakeMachine) CanStop() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.canStop
}

func (f *fakeMachine) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	if f.stopEnds {
		f.terminal = true
	}
	return nil
}

func (f *fakeMachine) set(fn func(*fakeMachine)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

const testStopTimeout = 100 * time.Millisecond

func TestStopMachineTerminalSkipsStop(t *testing.T) {
	m := &fakeMachine{terminal: true, canStop: true}
	if err := stopMachine(m, nil, testStopTimeout); err != nil {
		t.Fatal(err)
	}
	if m.stops != 0 {
		t.Fatalf("Stop called %d times for a terminal machine", m.stops)
	}
}

func TestStopMachineRejectedStopIsAnError(t *testing.T) {
	rejected := errors.New("framework rejected the stop")
	m := &fakeMachine{canStop: true, stopErr: rejected}
	err := stopMachine(m, make(chan struct{}), testStopTimeout)
	if !errors.Is(err, rejected) {
		t.Fatalf("error %v, want the rejection", err)
	}
}

func TestStopMachineRejectedStopOfAnEndedMachineSucceeds(t *testing.T) {
	m := &endsDuringStop{fakeMachine: fakeMachine{canStop: true, stopErr: errors.New("already stopped")}}
	if err := stopMachine(m, make(chan struct{}), testStopTimeout); err != nil {
		t.Fatal(err)
	}
}

// endsDuringStop becomes terminal inside Stop and also rejects the stop.
type endsDuringStop struct{ fakeMachine }

func (e *endsDuringStop) Stop() error {
	err := e.fakeMachine.Stop()
	e.set(func(f *fakeMachine) { f.terminal = true })
	return err
}

func TestStopMachineTimeoutWithoutTerminalState(t *testing.T) {
	m := &fakeMachine{canStop: true}
	err := stopMachine(m, make(chan struct{}), testStopTimeout)
	if !errors.Is(err, errStopTimeout) {
		t.Fatalf("error %v, want errStopTimeout", err)
	}
}

func TestStopMachineWaitsWithoutCanStop(t *testing.T) {
	m := &fakeMachine{}
	exited := make(chan struct{})
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(exited)
	}()
	if err := stopMachine(m, exited, testStopTimeout); err != nil {
		t.Fatal(err)
	}
	if m.stops != 0 {
		t.Fatalf("Stop called %d times although CanStop is false", m.stops)
	}
}

func TestStopMachineNotificationClosed(t *testing.T) {
	m := &fakeMachine{canStop: true}
	exited := make(chan struct{})
	close(exited)
	if err := stopMachine(m, exited, testStopTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestStopMachineMissingNotificationUsesTheState(t *testing.T) {
	// The state-change notification never arrives, but the state read does.
	m := &fakeMachine{canStop: true, stopEnds: true}
	if err := stopMachine(m, make(chan struct{}), testStopTimeout); err != nil {
		t.Fatal(err)
	}
}

type fakeVM struct{ id string }

// TestReleaseAfterStopRetainsHandleUntilTheStopSucceeds covers the delete
// path: a rejected stop and a terminal-state timeout keep the tracked handle
// and skip the cleanup; the retry that succeeds cleans up exactly once.
func TestReleaseAfterStopRetainsHandleUntilTheStopSucceeds(t *testing.T) {
	var mu sync.Mutex
	v := &fakeVM{id: "sb-1"}
	other := &fakeVM{id: "sb-2"}
	tracked := map[string]*fakeVM{v.id: v, other.id: other}
	m := &fakeMachine{canStop: true, stopErr: errors.New("stop rejected")}
	var released atomic.Int32
	run := func() error {
		return releaseAfterStop(&mu, tracked, v.id, v,
			func() error { return stopMachine(m, make(chan struct{}), testStopTimeout) },
			func() error { released.Add(1); return nil })
	}

	// 1: the framework rejects the stop.
	if err := run(); err == nil {
		t.Fatal("a rejected stop returned nil")
	}
	if tracked[v.id] != v || released.Load() != 0 {
		t.Fatalf("handle tracked=%v released=%d after a rejected stop", tracked[v.id] == v, released.Load())
	}

	// 2: the stop is accepted but the VM never reaches a terminal state.
	m.set(func(f *fakeMachine) { f.stopErr = nil })
	err := run()
	if !errors.Is(err, errStopTimeout) {
		t.Fatalf("error %v, want errStopTimeout", err)
	}
	if tracked[v.id] != v || released.Load() != 0 {
		t.Fatalf("handle tracked=%v released=%d after a stop timeout", tracked[v.id] == v, released.Load())
	}

	// 3: the retry succeeds and cleans up once.
	m.set(func(f *fakeMachine) { f.stopEnds = true })
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, ok := tracked[v.id]; ok || released.Load() != 1 {
		t.Fatalf("handle tracked=%v released=%d after the successful stop", ok, released.Load())
	}
	if tracked[other.id] != other {
		t.Fatal("another sandbox's handle was removed")
	}
}

func TestReleaseAfterStopReturnsReleaseError(t *testing.T) {
	var mu sync.Mutex
	v := &fakeVM{id: "sb-1"}
	tracked := map[string]*fakeVM{v.id: v}
	failure := errors.New("remove failed")
	err := releaseAfterStop(&mu, tracked, v.id, v, func() error { return nil }, func() error { return failure })
	if !errors.Is(err, failure) {
		t.Fatalf("error %v, want the release error", err)
	}
	// The VM is stopped, so its handle is gone; a repeated delete removes the
	// files again without a handle.
	if _, ok := tracked[v.id]; ok {
		t.Fatal("handle of a stopped VM is still tracked")
	}
}
