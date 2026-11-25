// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"context"
	"sync"
	"time"
)

// machineWatcher belongs to one VM attempt. Exited signals a terminal state.
// Done signals that the notification goroutine has returned.
type machineWatcher struct {
	exited   chan struct{}
	done     chan struct{}
	cancel   context.CancelFunc
	exitOnce sync.Once
	stopper  machineStopper
}

func watchMachine[S any](notify <-chan S, terminal func(S) bool) *machineWatcher {
	ctx, cancel := context.WithCancel(context.Background())
	w := &machineWatcher{exited: make(chan struct{}), done: make(chan struct{}), cancel: cancel}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-ctx.Done():
				return
			case state, ok := <-notify:
				if !ok {
					return
				}
				if terminal(state) {
					w.signalExit()
					return
				}
			}
		}
	}()
	return w
}

func (w *machineWatcher) signalExit() {
	w.exitOnce.Do(func() { close(w.exited) })
}

// stop releases the watcher only after stopMachine confirms termination.
func (w *machineWatcher) stop(m stoppableMachine, timeout time.Duration) error {
	if err := w.stopper.stopMachine(m, w.exited, timeout); err != nil {
		return err
	}
	// A state read can confirm termination before a notification arrives.
	w.signalExit()
	w.cancel()
	<-w.done
	return nil
}
