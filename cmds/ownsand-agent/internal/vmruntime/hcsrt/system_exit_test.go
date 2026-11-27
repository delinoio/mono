// SPDX-License-Identifier: FSL-1.1-ALv2

package hcsrt

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitExitRetriesInconclusiveStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "running"},
		{name: "transient error", err: errors.New("HCS temporarily unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*stopTimeout)
				defer cancel()
				end, _ := ctx.Deadline()
				var calls atomic.Int32
				release := make(chan struct{})
				s := newSystemExit("vm-id", func(id string, deadline time.Time) (bool, error) {
					call := calls.Add(1)
					wantDeadline := time.Now().Add(stopTimeout)
					if end.Before(wantDeadline) {
						wantDeadline = end
					}
					if id != "vm-id" || !deadline.Equal(wantDeadline) {
						t.Errorf("query %d got ID %q and deadline %v, want vm-id and %v", call, id, deadline, wantDeadline)
					}
					if call == 1 {
						<-release
						return false, tc.err
					}
					return call == 3, nil
				})
				done := make(chan error, 1)
				go func() { done <- s.waitExit(ctx) }()
				synctest.Wait()
				if calls.Load() != 0 {
					t.Errorf("queries before disconnect = %d, want 0", calls.Load())
				}
				close(s.disconnected)
				synctest.Wait()
				time.Sleep(stopTimeout + time.Millisecond)
				synctest.Wait()
				if calls.Load() != 1 || s.exitConfirmed() {
					t.Errorf("blocked query: calls = %d, exit confirmed = %v", calls.Load(), s.exitConfirmed())
				}
				close(release)
				synctest.Wait()
				time.Sleep(exitRetryInterval - time.Millisecond)
				synctest.Wait()
				if calls.Load() != 1 || s.exitConfirmed() {
					t.Errorf("retry delay: calls = %d, exit confirmed = %v", calls.Load(), s.exitConfirmed())
				}
				time.Sleep(time.Millisecond)
				synctest.Wait()
				if calls.Load() != 2 || s.exitConfirmed() {
					t.Errorf("first retry: calls = %d, exit confirmed = %v", calls.Load(), s.exitConfirmed())
				}
				time.Sleep(exitRetryInterval)
				synctest.Wait()
				select {
				case err := <-done:
					if err != nil || calls.Load() != 3 || !s.exitConfirmed() {
						t.Errorf("waitExit = %v, calls = %d, exit confirmed = %v", err, calls.Load(), s.exitConfirmed())
					}
				default:
					t.Fatal("waitExit did not finish on the third query")
				}
			})
		})
	}
}

func TestWaitExitStopsAfterTerminalSignal(t *testing.T) {
	for _, phase := range []string{"callback only", "in-flight query", "retry delay"} {
		for _, outcome := range []string{"callback", "handle closure", "cancellation"} {
			t.Run(phase+"/"+outcome, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					release := make(chan struct{})
					defer close(release)
					var calls atomic.Int32
					statusErr := errors.New("HCS temporarily unavailable")
					s := newSystemExit("vm-id", func(string, time.Time) (bool, error) {
						calls.Add(1)
						if phase == "in-flight query" {
							<-release
						}
						return false, statusErr
					})
					if phase != "callback only" {
						close(s.disconnected)
					}
					done := make(chan error, 1)
					go func() { done <- s.waitExit(ctx) }()
					synctest.Wait()

					var wantErr error
					switch outcome {
					case "callback":
						s.markExited()
					case "handle closure":
						close(s.closed)
						wantErr = errSystemHandleClosed
					case "cancellation":
						cancel()
						wantErr = context.Canceled
					}
					synctest.Wait()
					select {
					case err := <-done:
						if !errors.Is(err, wantErr) {
							t.Errorf("waitExit = %v, want %v", err, wantErr)
						}
						if phase == "retry delay" && outcome == "cancellation" && !errors.Is(err, statusErr) {
							t.Errorf("waitExit = %v, want the latest query error", err)
						}
					default:
						t.Fatal("waitExit did not finish on the terminal signal")
					}
					time.Sleep(2 * exitRetryInterval)
					synctest.Wait()
					wantCalls := int32(1)
					if phase == "callback only" {
						wantCalls = 0
					}
					if calls.Load() != wantCalls || s.exitConfirmed() != (outcome == "callback") {
						t.Errorf("calls = %d, want %d, exit confirmed = %v after %s", calls.Load(), wantCalls, s.exitConfirmed(), outcome)
					}
				})
			})
		}
	}
}
