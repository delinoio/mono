// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"log/slog"
	"testing"
	"time"
)

// TestRunTunnelJoinsAfterStopVMClearedNetDone stops the VM before the tunnel
// worker body runs. stopVM clears v.netDone first; the worker must still
// close the channel that stopVM waits on and must not panic.
func TestRunTunnelJoinsAfterStopVMClearedNetDone(t *testing.T) {
	r := &Runtime{logger: slog.Default()}
	v := &vm{
		id:       "sandbox-test",
		exited:   make(chan struct{}),
		stopping: make(chan struct{}),
	}
	done := make(chan struct{})
	v.mu.Lock()
	v.netDone = done
	v.mu.Unlock()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		r.stopVM(v)
	}()

	// Wait until stopVM has taken the channel and cleared the field.
	deadline := time.After(5 * time.Second)
	for {
		v.mu.Lock()
		cleared := v.netDone == nil
		v.mu.Unlock()
		if cleared {
			break
		}
		select {
		case <-deadline:
			t.Fatal("stopVM did not clear netDone")
		case <-time.After(time.Millisecond):
		}
	}

	// The worker starts only now, after the teardown began.
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		r.runTunnel(v, nil, nil, done)
	}()

	select {
	case <-workerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("runTunnel did not return after stopVM")
	}
	select {
	case <-done:
	default:
		t.Fatal("runTunnel did not close its completion channel")
	}
	select {
	case <-stopped:
	case <-time.After(stopTimeout + 5*time.Second):
		t.Fatal("stopVM did not return")
	}
}
