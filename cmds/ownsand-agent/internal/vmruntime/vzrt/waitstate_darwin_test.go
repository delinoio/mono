// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build darwin && cgo

package vzrt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/Code-Hex/vz/v3"
)

func TestWaitStateResume(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state vz.VirtualMachineState
		err   error
	}{
		{name: "running", state: vz.VirtualMachineStateRunning},
		{name: "stopped", state: vz.VirtualMachineStateStopped, err: errVMTerminalState},
		{name: "error", state: vz.VirtualMachineStateError, err: errVMTerminalState},
		{name: "stalled", state: vz.VirtualMachineStateResuming, err: context.DeadlineExceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				ctx, cancel := context.WithTimeout(context.Background(), snapshotResumeTimeout)
				defer cancel()
				state := func() vz.VirtualMachineState {
					if time.Since(start) < time.Millisecond {
						return vz.VirtualMachineStateResuming
					}
					return tc.state
				}
				err := waitState(ctx, state, vz.VirtualMachineStateRunning)
				if !errors.Is(err, tc.err) {
					t.Fatalf("waitState returned %v, want %v", err, tc.err)
				}
				if tc.err == errVMTerminalState && !strings.Contains(err.Error(), tc.state.String()) {
					t.Fatalf("waitState error %v does not report %v", err, tc.state)
				}
				elapsed := time.Since(start)
				if tc.err == context.DeadlineExceeded {
					if elapsed != snapshotResumeTimeout {
						t.Fatalf("stalled wait took %v, want %v", elapsed, snapshotResumeTimeout)
					}
				} else if elapsed >= snapshotResumeTimeout {
					t.Fatalf("transition to %v waited until the deadline", tc.state)
				}
			})
		})
	}
}
