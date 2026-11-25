// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"errors"
	"testing"
	"time"
)

type watchTestState uint8

const (
	watchTestRunning watchTestState = iota
	watchTestStopped
	watchTestError
)

func watchTestTerminal(state watchTestState) bool {
	return state == watchTestStopped || state == watchTestError
}

func TestMachineWatchReleasesEachDiscardedAttempt(t *testing.T) {
	for range 3 {
		notify := make(chan watchTestState)
		w := watchMachine(notify, watchTestTerminal)
		t.Cleanup(w.cancel)
		m := &fakeMachine{terminal: true}
		for range 2 {
			if err := w.stop(m, testStopTimeout); err != nil {
				t.Fatal(err)
			}
		}
		if m.stops != 0 {
			t.Fatal("cleanup called Stop for a machine that never started")
		}
		select {
		case <-w.done:
		default:
			t.Fatal("cleanup returned while the discarded machine's watcher was still active")
		}
	}
}

func TestMachineWatchRetainedWhenStopFails(t *testing.T) {
	rejected := errors.New("framework rejected the stop")
	m := &fakeMachine{canStop: true, stopErr: rejected}
	notify := make(chan watchTestState)
	w := watchMachine(notify, watchTestTerminal)
	t.Cleanup(w.cancel)

	if err := w.stop(m, testStopTimeout); !errors.Is(err, rejected) {
		t.Fatalf("stop error = %v, want %v", err, rejected)
	}
	select {
	case <-w.done:
		t.Fatal("the watcher exited after a rejected stop")
	case <-w.exited:
		t.Fatal("the rejected stop was reported as terminal")
	case notify <- watchTestRunning:
	case <-time.After(time.Second):
		t.Fatal("the retained watcher did not read a state change")
	}

	m.set(func(f *fakeMachine) {
		f.stopErr = nil
		f.stopEnds = true
	})
	if err := w.stop(m, testStopTimeout); err != nil {
		t.Fatal(err)
	}
	select {
	case <-w.exited:
	default:
		t.Fatal("the terminal state observed by the stop retry did not signal exit")
	}
	select {
	case <-w.done:
	default:
		t.Fatal("the stop retry did not release the watcher")
	}
}

func TestMachineWatchSignalsTerminalEvents(t *testing.T) {
	for name, state := range map[string]watchTestState{"stopped": watchTestStopped, "error": watchTestError} {
		t.Run(name, func(t *testing.T) {
			notify := make(chan watchTestState, 2)
			w := watchMachine(notify, watchTestTerminal)
			t.Cleanup(w.cancel)
			notify <- watchTestRunning
			notify <- state
			select {
			case <-w.done:
			case <-time.After(time.Second):
				t.Fatal("the terminal event did not end the watcher")
			}
			select {
			case <-w.exited:
			default:
				t.Fatal("the terminal event did not signal exit")
			}
		})
	}
}

func TestMachineWatchCancellationDoesNotSignalExit(t *testing.T) {
	w := watchMachine(make(chan watchTestState), watchTestTerminal)
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(time.Second):
		t.Fatal("cancellation did not end the watcher")
	}
	select {
	case <-w.exited:
		t.Fatal("cancellation was reported as a terminal event")
	default:
	}
}
