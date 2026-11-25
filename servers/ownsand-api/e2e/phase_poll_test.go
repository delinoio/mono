// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func pollPhaseContextErr(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The deadline can pass before the context timer runs.
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}

func pollPhase(ctx context.Context, interval time.Duration, poll func(context.Context) (bool, error)) error {
	for {
		if err := pollPhaseContextErr(ctx); err != nil {
			return err
		}
		done, pollErr := poll(ctx)
		if err := pollPhaseContextErr(ctx); err != nil {
			if pollErr != nil && !errors.Is(err, pollErr) {
				return fmt.Errorf("%w (last poll error: %w)", err, pollErr)
			}
			return err
		}
		if done || pollErr != nil {
			return pollErr
		}

		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
	}
}

func TestWaitForBillingStateRejectsLateSuccess(t *testing.T) {
	for _, want := range []ownsandv1.SandboxState{
		ownsandv1.SandboxState_SANDBOX_STATE_PAUSED,
		ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
	} {
		t.Run(want.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				calls := 0
				err := waitForBillingState(t.Context(), want, func(ctx context.Context) (*ownsandv1.Sandbox, error) {
					calls++
					<-ctx.Done()
					return &ownsandv1.Sandbox{State: want}, nil
				})
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "last state "+want.String()) {
					t.Fatalf("wait for %v returned %v, want deadline with the last state", want, err)
				}
				if elapsed := time.Since(start); elapsed != time.Minute || calls != 1 {
					t.Fatalf("after %v and %d reads, want 1 minute and 1 read", elapsed, calls)
				}
			})
		})
	}
}

func TestWaitForBillingStateStopsDuringRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		parentEnds time.Duration
		cancel     bool
		wantErr    error
	}{
		{"phase deadline", time.Minute, false, context.DeadlineExceeded},
		{"earlier parent deadline", 50 * time.Millisecond, false, context.DeadlineExceeded},
		{"parent cancellation", 50 * time.Millisecond, true, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent := t.Context()
				if tc.cancel {
					ctx, cancel := context.WithCancel(parent)
					defer cancel()
					parent = ctx
					time.AfterFunc(tc.parentEnds, cancel)
				} else if tc.parentEnds < time.Minute {
					ctx, cancel := context.WithTimeout(parent, tc.parentEnds)
					defer cancel()
					parent = ctx
				}
				start := time.Now()
				calls := 0
				err := waitForBillingState(parent, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, func(ctx context.Context) (*ownsandv1.Sandbox, error) {
					calls++
					if tc.parentEnds == time.Minute {
						time.Sleep(time.Minute - 100*time.Millisecond)
					}
					return &ownsandv1.Sandbox{State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING}, nil
				})
				if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), "last state SANDBOX_STATE_RUNNING") {
					t.Fatalf("wait returned %v, want %v with the last state", err, tc.wantErr)
				}
				if elapsed := time.Since(start); elapsed != tc.parentEnds || calls != 1 {
					t.Fatalf("after %v and %d reads, want %v and 1 read", elapsed, calls, tc.parentEnds)
				}
			})
		})
	}
}

func TestWaitForBillingStateGivesEachTransitionItsOwnBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, want := range []ownsandv1.SandboxState{
			ownsandv1.SandboxState_SANDBOX_STATE_PAUSED,
			ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
		} {
			deadline := time.Now().Add(time.Minute)
			calls := 0
			err := waitForBillingState(t.Context(), want, func(ctx context.Context) (*ownsandv1.Sandbox, error) {
				calls++
				if actual, ok := ctx.Deadline(); !ok || !actual.Equal(deadline) {
					t.Fatalf("deadline = (%v, %v), want %v", actual, ok, deadline)
				}
				switch calls {
				case 1:
					return &ownsandv1.Sandbox{}, nil
				case 2:
					return nil, errors.New("temporary read failure")
				default:
					return &ownsandv1.Sandbox{State: want}, nil
				}
			})
			if err != nil || calls != 3 {
				t.Fatalf("wait for %v returned %v after %d reads, want success after 3 reads", want, err, calls)
			}
		}
	})
}

func TestWaitForExportVerificationRejectsLateSuccess(t *testing.T) {
	for _, stallDatabase := range []bool{true, false} {
		name := "snapshot RPC"
		if stallDatabase {
			name = "database query"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				start := time.Now()
				rpcCalls := 0
				err := waitForExportVerification(t.Context(), start.Add(exportVerifyWait), func(ctx context.Context) (bool, error) {
					if stallDatabase {
						<-ctx.Done()
					}
					return true, nil
				}, func(ctx context.Context) (ownsandv1.SnapshotExportState, error) {
					rpcCalls++
					if stallDatabase {
						t.Fatal("snapshot RPC started after database read exhausted the deadline")
					}
					<-ctx.Done()
					return ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED, nil
				})
				wantCalls, wantState := 1, "SNAPSHOT_EXPORT_STATE_EXPORT_FAILED"
				if stallDatabase {
					wantCalls, wantState = 0, "unobserved"
				}
				if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "matching export verified = true, tampered export state = "+wantState) {
					t.Fatalf("wait returned %v, want deadline with last export observations", err)
				}
				if elapsed := time.Since(start); elapsed != exportVerifyWait || rpcCalls != wantCalls {
					t.Fatalf("after %v and %d RPC calls, want %v and %d calls", elapsed, rpcCalls, exportVerifyWait, wantCalls)
				}
			})
		})
	}
}

func TestWaitForExportVerificationStopsDuringRetry(t *testing.T) {
	for _, tc := range []struct {
		name           string
		initialRead    time.Duration
		cancelParentAt time.Duration
		wantElapsed    time.Duration
		wantErr        error
	}{
		{"phase deadline", exportVerifyWait - 100*time.Millisecond, 0, exportVerifyWait, context.DeadlineExceeded},
		{"parent cancellation", 0, 100 * time.Millisecond, 100 * time.Millisecond, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				parent, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.cancelParentAt > 0 {
					time.AfterFunc(tc.cancelParentAt, cancel)
				}
				start := time.Now()
				dbCalls, rpcCalls := 0, 0
				err := waitForExportVerification(parent, start.Add(exportVerifyWait), func(context.Context) (bool, error) {
					dbCalls++
					return false, nil
				}, func(context.Context) (ownsandv1.SnapshotExportState, error) {
					rpcCalls++
					time.Sleep(tc.initialRead)
					return ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED, nil
				})
				if !errors.Is(err, tc.wantErr) || !strings.Contains(err.Error(), "matching export verified = false, tampered export state = SNAPSHOT_EXPORT_STATE_EXPORTED") {
					t.Fatalf("wait returned %v, want %v with last export observations", err, tc.wantErr)
				}
				if elapsed := time.Since(start); elapsed != tc.wantElapsed || dbCalls != 1 || rpcCalls != 1 {
					t.Fatalf("after %v, %d queries and %d RPCs, want %v and one call per read", elapsed, dbCalls, rpcCalls, tc.wantElapsed)
				}
			})
		})
	}
}

func TestWaitForExportVerificationPreservesReadResults(t *testing.T) {
	readFailure := errors.New("read failed")
	for _, tc := range []struct {
		name     string
		dbErr    error
		rpcErr   error
		wantRPCs int
		wantText string
	}{
		{name: "timely success", wantRPCs: 2},
		{"database error", readFailure, nil, 0, "read matching export verification"},
		{"RPC error", nil, readFailure, 1, "GetSnapshot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dbCalls, rpcCalls := 0, 0
				err := waitForExportVerification(t.Context(), time.Now().Add(exportVerifyWait), func(ctx context.Context) (bool, error) {
					dbCalls++
					return dbCalls > 1, tc.dbErr
				}, func(ctx context.Context) (ownsandv1.SnapshotExportState, error) {
					rpcCalls++
					return ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED, tc.rpcErr
				})
				if tc.wantText == "" {
					if err != nil || dbCalls != 2 {
						t.Fatalf("wait returned %v after %d queries, want success after 2 queries", err, dbCalls)
					}
				} else if !errors.Is(err, readFailure) || !strings.Contains(err.Error(), tc.wantText) || dbCalls != 1 {
					t.Fatalf("wait returned %v after %d queries, want %q and the read error after 1 query", err, dbCalls, tc.wantText)
				}
				if rpcCalls != tc.wantRPCs {
					t.Fatalf("got %d RPC calls, want %d", rpcCalls, tc.wantRPCs)
				}
			})
		})
	}
}

type deadlineWithoutTimer struct {
	context.Context
	deadline time.Time
}

func (c deadlineWithoutTimer) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func TestPollPhaseRejectsSuccessAtTheDeadlineBeforeTheContextTimerRuns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := deadlineWithoutTimer{t.Context(), time.Now().Add(time.Millisecond)}
		err := pollPhase(ctx, time.Second, func(ctx context.Context) (bool, error) {
			time.Sleep(time.Millisecond)
			return true, nil
		})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("poll returned %v, want deadline at %v", err, ctx.deadline)
		}
	})
}
