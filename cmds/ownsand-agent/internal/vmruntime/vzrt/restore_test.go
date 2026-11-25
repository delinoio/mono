// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"context"
	"errors"
	"testing"
)

func TestFailedRestoreAfterTerminalNotification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cancelBefore bool
		cancelOnStop bool
	}{
		{name: "active create"},
		{name: "canceled before readiness returns", cancelBefore: true},
		{name: "canceled during stop", cancelOnStop: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelBefore {
				cancel()
			}

			// The guest probe has not returned when the VM's terminal
			// notification wins. Its error does not contain the context error.
			ready := make(chan error)
			exited := make(chan struct{})
			close(exited)
			if err := waitGuestResult(ready, exited); err == nil || errors.Is(err, context.Canceled) {
				t.Fatalf("readiness error = %v, want a terminal VM error", err)
			}

			stopped, discarded := false, false
			err := finishFailedRestore(ctx, func() error {
				if tc.cancelOnStop {
					cancel()
				}
				stopped = true
				return nil
			}, func() {
				if !stopped {
					t.Fatal("discard ran before confirmed stop")
				}
				discarded = true
			})
			if !errors.Is(err, ctx.Err()) {
				t.Fatalf("failed restore result = %v, want %v", err, ctx.Err())
			}
			if want := tc.cancelBefore || tc.cancelOnStop; !stopped || discarded != want {
				t.Fatalf("stop ran = %t, discard ran = %t, want stop = true and discard = %t", stopped, discarded, want)
			}
		})
	}
}

func TestCanceledRestoreKeepsVMWhenStopIsUnconfirmed(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stopFailure := errors.New("stop request failed")
	err := finishFailedRestore(ctx, func() error { return stopFailure }, func() {
		t.Fatal("discard ran with an unconfirmed stop")
	})
	for _, want := range []error{context.Canceled, stopFailure, ErrVMMayRun} {
		if !errors.Is(err, want) {
			t.Errorf("failed restore result = %v, want %v", err, want)
		}
	}
}
