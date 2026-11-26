// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const recoveryLookupFailure = "first relay failure before acceptance"

func scriptRecoveryLookup(cp *fakeCP, attach, authFailure bool) {
	cp.authScript = func(n int, _ *ownsandv1.AuthorizeRelayRequest) authAction {
		if authFailure && n == 2 {
			return authAction{err: failedPrecondition()}
		}
		return authAction{}
	}
	cp.relayScript = func(method string, n int) relayAction {
		if (method == "AttachExec") == attach {
			switch n {
			case 1:
				return relayAction{err: connect.NewError(connect.CodeUnavailable, errors.New(recoveryLookupFailure))}
			case 2:
				return relayAction{err: failedPrecondition()}
			}
		}
		// After a pre-start Exec interruption, the fake has no exec record.
		// Let the first AttachExec return not_found and trigger a resend.
		return relayAction{}
	}
}

func TestExecRecoveryLookupCancellation(t *testing.T) {
	tests := []struct {
		name          string
		attach        bool
		authFailure   bool
		callerError   error
		wantExecCalls int
		wantAttach    int
	}{
		{name: "exec authorization", authFailure: true, wantExecCalls: 1},
		{name: "attach authorization", attach: true, authFailure: true, wantAttach: 1},
		{name: "resend stream", wantExecCalls: 2, wantAttach: 1},
		{name: "attach stream", attach: true, wantAttach: 2},
		{name: "caller cancellation", callerError: context.Canceled, wantExecCalls: 2, wantAttach: 1},
		{name: "caller deadline", attach: true, authFailure: true, callerError: context.DeadlineExceeded, wantAttach: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cp := newFakeCP(t)
			scriptRecoveryLookup(cp, tc.attach, tc.authFailure)
			entered, canceled := make(chan struct{}, 1), make(chan struct{}, 1)
			var lookups atomic.Int32
			cp.beforeGetSandbox = func(ctx context.Context) error {
				first := lookups.Add(1) == 1
				if first {
					entered <- struct{}{}
				}
				<-ctx.Done()
				if first {
					canceled <- struct{}{}
				}
				return ctx.Err()
			}
			window := 700 * time.Millisecond
			if tc.callerError != nil {
				window = 4 * time.Second
			}
			s := cp.handle(t, recoveryDeadlineClient(t, cp, window))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.callerError == context.DeadlineExceeded {
				deadlineCtx, endDeadline := context.WithTimeout(ctx, time.Second)
				defer endDeadline()
				ctx = deadlineCtx
			}
			id := mustID(t)
			type outcome struct {
				result *ExecResult
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				if tc.attach {
					result, err := s.AttachExec(ctx, id, AttachParams{})
					done <- outcome{result, err}
					return
				}
				result, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
				done <- outcome{result, err}
			}()
			select {
			case <-entered:
			case got := <-done:
				t.Fatalf("operation ended before GetSandbox: %+v", got)
			case <-time.After(3 * time.Second):
				t.Fatal("GetSandbox did not start")
			}
			if tc.callerError == context.Canceled {
				cancel()
			}
			limit := time.NewTimer(window + time.Second)
			defer limit.Stop()
			select {
			case got := <-done:
				if got.result != nil {
					t.Fatalf("unexpected result: %+v", got.result)
				}
				if tc.callerError != nil {
					if got.err != tc.callerError {
						t.Fatalf("error = %v, want %v", got.err, tc.callerError)
					}
				} else {
					var dp *DataPathError
					var prior *connect.Error
					if !errors.As(got.err, &dp) || !errors.Is(got.err, errRecoveryDeadline) ||
						!errors.As(got.err, &prior) || prior.Code() != connect.CodeUnavailable || prior.Message() != recoveryLookupFailure {
						t.Fatalf("want a recovery deadline error wrapping the prior relay failure, got %v", got.err)
					}
				}
			case <-limit.C:
				t.Fatal("operation did not end within its recovery budget")
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				t.Fatal("the server did not observe lookup cancellation")
			}
			if n := lookups.Load(); n != 1 {
				t.Fatalf("GetSandbox calls = %d, want 1", n)
			}
			if n := cp.relayCount("Exec"); n != tc.wantExecCalls {
				t.Fatalf("Exec calls = %d, want %d", n, tc.wantExecCalls)
			}
			if n := cp.relayCount("AttachExec"); n != tc.wantAttach {
				t.Fatalf("AttachExec calls = %d, want %d", n, tc.wantAttach)
			}
		})
	}
}

func TestExecRecoveryLookupTimelyTerminal(t *testing.T) {
	cp := newFakeCP(t)
	scriptRecoveryLookup(cp, false, true)
	cp.beforeGetSandbox = func(context.Context) error {
		cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
		return nil
	}
	cp.endReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
	s := cp.handle(t, cp.client(t, TransportRelay))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
	if err != nil || result == nil || result.Kind != ExecResultSandboxEnded || result.EndReason != cp.endReason {
		t.Fatalf("Exec = (%+v, %v), want sandbox ended with %v", result, err, cp.endReason)
	}
}

func TestExecRecoveryLookupTimelyPause(t *testing.T) {
	cp := newFakeCP(t)
	scriptRecoveryLookup(cp, false, false)
	cp.agent.run = lineRun(1, 0)
	c := recoveryDeadlineClient(t, cp, 500*time.Millisecond)
	var mu sync.Mutex
	var resumeAt time.Time
	cp.beforeGetSandbox = func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		if resumeAt.IsZero() {
			cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
			resumeAt = time.Now().Add(c.timing.recoveryWindow + 50*time.Millisecond)
		} else if time.Now().After(resumeAt) {
			cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
		}
		return nil
	}
	requests := make(chan *ownsandv1.ExecRequest, 4)
	cp.onExecRequest = func(req *ownsandv1.ExecRequest) {
		requests <- proto.CloneOf(req)
	}
	s := cp.handle(t, c)
	result, output := runExec(t, s)
	checkLines(t, result, output, 1)
	if n := len(requests); n != 3 {
		t.Fatalf("Exec calls = %d, want 3", n)
	}
	first := <-requests
	for range 2 {
		if got := <-requests; !proto.Equal(got, first) {
			t.Fatalf("resend = %v, want identical request %v", got, first)
		}
	}
}

// recoveryLookupReply simulates a transport that returns a response even
// after the lookup context ends.
type recoveryLookupReply struct {
	ownsandv1connect.SandboxServiceClient
	state             ownsandv1.SandboxState
	calls             int
	afterCancellation func()
}

func (r *recoveryLookupReply) GetSandbox(ctx context.Context, _ *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	r.calls++
	<-ctx.Done()
	if r.afterCancellation != nil {
		r.afterCancellation()
	}
	sb := &ownsandv1.Sandbox{State: r.state}
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: sb}), nil
}

func TestExecRecoveryLookupIgnoresLateState(t *testing.T) {
	for _, state := range []ownsandv1.SandboxState{
		ownsandv1.SandboxState_SANDBOX_STATE_PAUSED,
		ownsandv1.SandboxState_SANDBOX_STATE_DELETED,
	} {
		t.Run(state.String(), func(t *testing.T) {
			t.Parallel()
			cp := newFakeCP(t)
			scriptRecoveryLookup(cp, false, false)
			c := recoveryDeadlineClient(t, cp, 500*time.Millisecond)
			lookup := &recoveryLookupReply{state: state}
			c.sandboxes = lookup
			s := cp.handle(t, c)
			initialInfo := s.Info()
			ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
			defer cancel()
			lookup.afterCancellation = cancel
			result, err := s.Exec(ctx, ExecParams{Command: []string{"run"}})
			if result != nil || !errors.Is(err, errRecoveryDeadline) || lookup.calls != 1 || cp.relayCount("Exec") != 2 {
				t.Fatalf("Exec = (%+v, %v), GetSandbox calls = %d, Exec calls = %d", result, err, lookup.calls, cp.relayCount("Exec"))
			}
			if s.Info() != initialInfo {
				t.Fatalf("late reply updated sandbox info to %v", s.Info())
			}
		})
	}
}

func TestExecRecoveryLookupExpiredOrInactive(t *testing.T) {
	cp := newFakeCP(t)
	c := recoveryDeadlineClient(t, cp, 5*time.Millisecond)
	lookup := &recoveryLookupReply{state: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING}
	c.sandboxes = lookup
	op := execOp{s: cp.handle(t, c), rec: newRecovery(0)}
	last := errors.New(recoveryLookupFailure)
	op.rec.fail(last)
	sb, err := op.lookupSandbox(context.Background())
	if sb != nil || !errors.Is(err, errRecoveryDeadline) || !errors.Is(err, last) || lookup.calls != 0 {
		t.Fatalf("expired lookup = (%v, %v), calls = %d", sb, err, lookup.calls)
	}

	op.rec.reset()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sb, err = op.lookupSandbox(ctx)
	if sb != nil || err != context.DeadlineExceeded || lookup.calls != 1 {
		t.Fatalf("lookup outside recovery = (%v, %v), calls = %d", sb, err, lookup.calls)
	}
}
