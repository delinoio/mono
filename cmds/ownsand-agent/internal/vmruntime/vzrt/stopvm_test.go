// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
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
	if err := new(machineStopper).stopMachine(m, nil, testStopTimeout); err != nil {
		t.Fatal(err)
	}
	if m.stops != 0 {
		t.Fatalf("Stop called %d times for a terminal machine", m.stops)
	}
}

func TestStopMachineRejectedStopIsAnError(t *testing.T) {
	rejected := errors.New("framework rejected the stop")
	m := &fakeMachine{canStop: true, stopErr: rejected}
	err := new(machineStopper).stopMachine(m, make(chan struct{}), testStopTimeout)
	if !errors.Is(err, rejected) {
		t.Fatalf("error %v, want the rejection", err)
	}
}

func TestStopMachineRejectedStopOfAnEndedMachineSucceeds(t *testing.T) {
	m := &endsDuringStop{fakeMachine: fakeMachine{canStop: true, stopErr: errors.New("already stopped")}}
	if err := new(machineStopper).stopMachine(m, make(chan struct{}), testStopTimeout); err != nil {
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
	err := new(machineStopper).stopMachine(m, make(chan struct{}), testStopTimeout)
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
	if err := new(machineStopper).stopMachine(m, exited, testStopTimeout); err != nil {
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
	if err := new(machineStopper).stopMachine(m, exited, testStopTimeout); err != nil {
		t.Fatal(err)
	}
}

func TestStopMachineMissingNotificationUsesTheState(t *testing.T) {
	// The state-change notification never arrives, but the state read does.
	m := &fakeMachine{canStop: true, stopEnds: true}
	if err := new(machineStopper).stopMachine(m, make(chan struct{}), testStopTimeout); err != nil {
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
	var stopper machineStopper
	run := func() error {
		return releaseAfterStop(&mu, tracked, v.id, v,
			func() error { return stopper.stopMachine(m, make(chan struct{}), testStopTimeout) },
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

func TestStopForCleanupTerminalKeepsThePrimaryError(t *testing.T) {
	primary := errors.New("first boot failed")
	m := &fakeMachine{terminal: true, canStop: true}
	err := stopForCleanup(primary, m, nil, testStopTimeout)
	if err != primary {
		t.Fatalf("error %v, want exactly the primary error", err)
	}
	if m.stops != 0 {
		t.Fatalf("Stop called %d times for a terminal machine", m.stops)
	}
}

func TestStopForCleanupStopReachesTerminal(t *testing.T) {
	primary := errors.New("first boot failed")
	m := &fakeMachine{canStop: true, stopEnds: true}
	if err := stopForCleanup(primary, m, make(chan struct{}), testStopTimeout); err != primary {
		t.Fatalf("error %v, want exactly the primary error", err)
	}
	if m.stops != 1 {
		t.Fatalf("Stop called %d times, want 1", m.stops)
	}
}

func TestStopForCleanupRejectedStopReportsBothWithoutWaiting(t *testing.T) {
	primary := errors.New("first boot failed")
	rejected := errors.New("framework rejected the stop")
	m := &fakeMachine{canStop: true, stopErr: rejected}
	// A nil-free but never closed channel: the cleanup must not wait for it.
	start := time.Now()
	err := stopForCleanup(primary, m, make(chan struct{}), time.Minute)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("cleanup waited %v after a rejected stop", elapsed)
	}
	for _, want := range []error{primary, rejected, ErrVMMayRun} {
		if !errors.Is(err, want) {
			t.Fatalf("error %v does not wrap %v", err, want)
		}
	}
}

func TestStopForCleanupAcceptedStopWithoutTerminalTimesOut(t *testing.T) {
	primary := errors.New("first boot failed")
	m := &fakeMachine{canStop: true}
	start := time.Now()
	err := stopForCleanup(primary, m, make(chan struct{}), testStopTimeout)
	if elapsed := time.Since(start); elapsed < testStopTimeout || elapsed > 5*time.Second {
		t.Fatalf("cleanup waited %v, want about %v", elapsed, testStopTimeout)
	}
	for _, want := range []error{primary, errStopTimeout, ErrVMMayRun} {
		if !errors.Is(err, want) {
			t.Fatalf("error %v does not wrap %v", err, want)
		}
	}
}

// Hook the native calls before they take the fake's state lock so a stop
// caller can still read the machine state while the request is blocked.
type stopHooksMachine struct {
	fakeMachine
	canStopHook func()
	stopHook    func()
}

func (m *stopHooksMachine) CanStop() bool {
	if m.canStopHook != nil {
		m.canStopHook()
	}
	return m.fakeMachine.CanStop()
}

func (m *stopHooksMachine) Stop() error {
	if m.stopHook != nil {
		m.stopHook()
	}
	return m.fakeMachine.Stop()
}

func TestStopMachineStalledRequestRetainsFilesAndHandle(t *testing.T) {
	for _, phase := range []string{"eligibility", "completion"} {
		t.Run(phase, func(t *testing.T) {
			dir := t.TempDir()
			disk := filepath.Join(dir, "disk.img")
			if err := os.WriteFile(disk, []byte("staged disk"), 0o600); err != nil {
				t.Fatal(err)
			}
			synctest.Test(t, func(t *testing.T) {
				unblock := make(chan struct{})
				resume := sync.OnceFunc(func() { close(unblock) })
				defer resume()
				var calls atomic.Int32
				stalled := func() {
					calls.Add(1)
					<-unblock
				}
				m := &stopHooksMachine{fakeMachine: fakeMachine{canStop: true, stopEnds: true}}
				if phase == "eligibility" {
					m.canStopHook = stalled
				} else {
					m.stopHook = stalled
				}

				w := watchMachine(make(chan watchTestState), watchTestTerminal)
				defer w.cancel()
				v := &fakeVM{id: "sb-1"}
				var mu sync.Mutex
				tracked := map[string]*fakeVM{v.id: v}
				var released bool
				clean := func() error {
					return releaseAfterStop(&mu, tracked, v.id, v, func() error {
						return w.stop(m, testStopTimeout)
					}, func() error {
						released = true
						return os.Remove(disk)
					})
				}
				for range 2 {
					start := time.Now()
					if err := clean(); !errors.Is(err, errStopTimeout) {
						t.Fatalf("cleanup error = %v, want errStopTimeout", err)
					}
					if elapsed := time.Since(start); elapsed != testStopTimeout {
						t.Fatalf("cleanup took %v, want %v", elapsed, testStopTimeout)
					}
					if tracked[v.id] != v || released {
						t.Fatal("cleanup released the VM while its stop request was pending")
					}
					if _, err := os.Stat(disk); err != nil {
						t.Fatalf("disk was not retained: %v", err)
					}
					select {
					case <-w.done:
						t.Fatal("the VM watcher was released before termination")
					default:
					}
				}

				// A later retry can finish after the original request ends.
				resume()
				synctest.Wait()
				if calls.Load() != 1 || m.stops != 1 {
					t.Fatalf("got %d stalled calls and %d Stop calls, want one each", calls.Load(), m.stops)
				}
				if err := clean(); err != nil {
					t.Fatal(err)
				}
				if tracked[v.id] != nil || !released {
					t.Fatal("the terminated VM was not released")
				}
				if _, err := os.Stat(disk); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("disk was not removed after termination: %v", err)
				}
			})
		})
	}
}

func TestStopMachineTerminalWhileCanStopIsBlocked(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var stopper machineStopper
		exited := make(chan struct{})
		unblock := make(chan struct{})
		resume := sync.OnceFunc(func() { close(unblock) })
		defer resume()
		m := &stopHooksMachine{
			fakeMachine: fakeMachine{canStop: true},
			canStopHook: func() { <-unblock },
		}
		results := make(chan error, 2)
		for range 2 {
			go func() { results <- stopper.stopMachine(m, exited, testStopTimeout) }()
		}
		synctest.Wait()
		close(exited)
		for range 2 {
			if err := <-results; err != nil {
				t.Fatalf("terminal notification did not unblock stop: %v", err)
			}
		}
		resume()
		synctest.Wait()
		if m.stops != 0 {
			t.Fatalf("Stop called %d times after the VM terminated", m.stops)
		}
	})
}

func TestStopForCleanupSharesDeadlineBetweenRequestAndTerminalWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		primary := errors.New("first boot failed")
		for _, delay := range []time.Duration{testStopTimeout / 2, 2 * testStopTimeout} {
			m := &stopHooksMachine{
				fakeMachine: fakeMachine{canStop: true},
				stopHook:    func() { time.Sleep(delay) },
			}
			start := time.Now()
			err := stopForCleanup(primary, m, make(chan struct{}), testStopTimeout)
			if elapsed := time.Since(start); elapsed != testStopTimeout {
				t.Fatalf("cleanup took %v with a %v request, want %v", elapsed, delay, testStopTimeout)
			}
			for _, want := range []error{primary, ErrVMMayRun, errStopTimeout} {
				if !errors.Is(err, want) {
					t.Fatalf("error %v does not wrap %v", err, want)
				}
			}
			// Let the fake callback return before the test exits its bubble.
			time.Sleep(delay)
		}
	})
}
