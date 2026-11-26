// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

var (
	retryStarted = &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Started{
		Started: &sandrunv1.DevSessionExecStarted{},
	}}
	retryKeepalive = &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Keepalive{
		Keepalive: &sandrunv1.DevSessionExecKeepalive{},
	}}
	retryExited = &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{
		Exited: &sandrunv1.DevSessionExecExited{Status: &sandrunv1.DevSessionExecExited_ExitCode{}},
	}}
)

type execRetryCall struct {
	initial        bool
	id             string
	stdout, stderr int64
}

type execRetryStep func(context.Context, int, func(*sandrunv1.DevSessionExecEvent) error) error

type execRetryFixture struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	step execRetryStep
	log  bytes.Buffer
	mu   sync.Mutex
	got  []execRetryCall
}

func (f *execRetryFixture) record(call execRetryCall) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	index := len(f.got)
	f.got = append(f.got, call)
	return index
}

func (f *execRetryFixture) ExecDevSession(ctx context.Context, req *connect.Request[sandrunv1.ExecDevSessionRequest], stream *connect.ServerStream[sandrunv1.ExecDevSessionResponse]) error {
	index := f.record(execRetryCall{initial: true, id: req.Msg.GetExecId()})
	return f.step(ctx, index, func(ev *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.ExecDevSessionResponse{Event: ev})
	})
}

func (f *execRetryFixture) AttachDevSessionExec(ctx context.Context, req *connect.Request[sandrunv1.AttachDevSessionExecRequest], stream *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error {
	index := f.record(execRetryCall{id: req.Msg.GetExecId(), stdout: req.Msg.GetStdoutOffset(), stderr: req.Msg.GetStderrOffset()})
	return f.step(ctx, index, func(ev *sandrunv1.DevSessionExecEvent) error {
		return stream.Send(&sandrunv1.AttachDevSessionExecResponse{Event: ev})
	})
}

func newExecRetryFixture(t *testing.T, step execRetryStep) (*Client, *execRetryFixture) {
	t.Helper()
	f := &execRetryFixture{step: step}
	_, handler := sandrunv1connect.NewDevSessionServiceHandler(f)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewJSONHandler(&f.log, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := New(Options{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: sandruntest.DefaultAPIKey, Logger: logger})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = timing{backoffMin: 25 * time.Millisecond, backoffMax: 100 * time.Millisecond, retryBudget: 500 * time.Millisecond}
	return c, f
}

func (f *execRetryFixture) calls() []execRetryCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]execRetryCall(nil), f.got...)
}

func (f *execRetryFixture) delays(t *testing.T) []time.Duration {
	t.Helper()
	var delays []time.Duration
	dec := json.NewDecoder(bytes.NewReader(f.log.Bytes()))
	for dec.More() {
		var entry struct {
			Msg   string
			Delay time.Duration
		}
		if err := dec.Decode(&entry); err != nil {
			t.Fatal(err)
		}
		if entry.Msg == "sandrun exec continues" {
			delays = append(delays, entry.Delay)
		}
	}
	return delays
}

func execRetryUnavailable() error {
	return connect.NewError(connect.CodeUnavailable, errors.New("temporarily unavailable"))
}

func requireExecRetryDelay(t *testing.T, got, want time.Duration) {
	t.Helper()
	if got < want*4/5 || got > want {
		t.Fatalf("retry delay = %s, want %s with up to 20%% jitter", got, want)
	}
}

func requireExecRetryError(t *testing.T, err error, id string, stdout, stderr int64, started bool) *ExecStreamError {
	t.Helper()
	var se *ExecStreamError
	if !errors.As(err, &se) || se.ExecID != id || se.StdoutOffset != stdout || se.StderrOffset != stderr ||
		se.Started != started || se.OutcomeUnknown {
		t.Fatalf("error = %#v, want ExecStreamError with ID %q, offsets (%d, %d), Started=%t and OutcomeUnknown=false",
			err, id, stdout, stderr, started)
	}
	return se
}

func TestExecRetryBudgetWithoutProgress(t *testing.T) {
	for _, tc := range []struct {
		name        string
		attach      bool
		unavailable bool
	}{
		{name: "exec clean"},
		{name: "exec unavailable", unavailable: true},
		{name: "attach clean", attach: true},
		{name: "attach unavailable", attach: true, unavailable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newExecRetryFixture(t, func(_ context.Context, index int, send func(*sandrunv1.DevSessionExecEvent) error) error {
				ev := retryStarted
				if index%2 == 1 {
					ev = retryKeepalive
				}
				if err := send(ev); err != nil {
					return err
				}
				if index > 0 && tc.unavailable {
					return execRetryUnavailable()
				}
				return nil
			})
			ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
			defer cancel()
			var events collected
			var stdout, stderr int64
			wantStarts := 1
			var err error
			if tc.attach {
				stdout, stderr, wantStarts = 11, 3, 0
				_, err = c.AttachExec(ctx, AttachParams{ExecID: "exec", StdoutOffset: stdout, StderrOffset: stderr, OnEvent: events.on})
			} else {
				_, err = c.Exec(ctx, ExecParams{ExecID: "exec", Command: []string{"true"}, OnEvent: events.on})
			}
			se := requireExecRetryError(t, err, "exec", stdout, stderr, true)
			if ctx.Err() != nil || events.started != wantStarts {
				t.Fatalf("caller = %v, starts = %d, want active caller and %d starts", ctx.Err(), events.started, wantStarts)
			}
			if tc.unavailable && (!connect.IsWireError(se.Err) || connect.CodeOf(se.Err) != connect.CodeUnavailable) {
				t.Fatalf("error = %v, want final Unavailable wire status", se.Err)
			}
			for index, call := range f.calls() {
				want := execRetryCall{initial: !tc.attach && index == 0, id: "exec", stdout: stdout, stderr: stderr}
				if call != want {
					t.Fatalf("call %d = %+v, want %+v", index, call, want)
				}
			}
			delays := f.delays(t)
			t.Logf("RPCs = %d, planned delays = %v, final code = %s", len(f.calls()), delays, connect.CodeOf(se.Err))
			if len(delays) < 5 {
				t.Fatalf("delays = %v, want backoff doubling and then repeating at the cap", delays)
			}
			for i, want := range []time.Duration{25, 50, 100, 100} {
				requireExecRetryDelay(t, delays[i], want*time.Millisecond)
			}
		})
	}
}

func TestExecRetryBudgetBoundsStalledAttach(t *testing.T) {
	for _, tc := range []struct {
		name      string
		sendFrame bool
		cancelRPC bool
	}{
		{name: "headers"},
		{name: "frames", sendFrame: true},
		{name: "caller cancellation", cancelRPC: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			stopped := make(chan struct{})
			c, f := newExecRetryFixture(t, func(rpcCtx context.Context, index int, send func(*sandrunv1.DevSessionExecEvent) error) error {
				if index == 0 {
					if err := send(retryStarted); err != nil {
						return err
					}
					if err := send(execRetryStdout(0, "abc")); err != nil {
						return err
					}
					return execRetryUnavailable()
				}
				if tc.sendFrame {
					if err := send(retryKeepalive); err != nil {
						return err
					}
				}
				if tc.cancelRPC {
					cancel()
				}
				<-rpcCtx.Done()
				close(stopped)
				return rpcCtx.Err()
			})
			c.timing.retryBudget = 180 * time.Millisecond
			start := time.Now()
			_, err := c.Exec(ctx, ExecParams{ExecID: "exec", Command: []string{"true"}})
			se := requireExecRetryError(t, err, "exec", 3, 0, true)
			if tc.cancelRPC {
				if !errors.Is(se.Err, context.Canceled) {
					t.Fatalf("error = %v, want caller cancellation", se.Err)
				}
			} else if ctx.Err() != nil || !connect.IsWireError(se.Err) || connect.CodeOf(se.Err) != connect.CodeUnavailable {
				t.Fatalf("caller = %v, error = %v, want active caller and original Unavailable status", ctx.Err(), se.Err)
			}
			select {
			case <-stopped:
			case <-time.After(time.Second):
				t.Fatal("server did not observe cancellation of the stalled attach")
			}
			calls := f.calls()
			if len(calls) != 2 || calls[0] != (execRetryCall{initial: true, id: "exec"}) ||
				calls[1] != (execRetryCall{id: "exec", stdout: 3}) {
				t.Fatalf("calls = %+v, want one exec and one attach at acknowledged offsets", calls)
			}
			t.Logf("stalled attach stopped in %s with error %v", time.Since(start), se.Err)
		})
	}
}

func TestExecRetryBudgetBoundsBackoff(t *testing.T) {
	c, f := newExecRetryFixture(t, func(context.Context, int, func(*sandrunv1.DevSessionExecEvent) error) error {
		return execRetryUnavailable()
	})
	c.timing.backoffMin = 5 * time.Second
	c.timing.backoffMax = c.timing.backoffMin
	c.timing.retryBudget = 80 * time.Millisecond
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	start := time.Now()
	_, err := c.Exec(ctx, ExecParams{ExecID: "exec", Command: []string{"true"}})
	se := requireExecRetryError(t, err, "exec", 0, 0, false)
	if ctx.Err() != nil || connect.CodeOf(se.Err) != connect.CodeUnavailable {
		t.Fatalf("caller = %v, error = %v, want active caller and Unavailable status", ctx.Err(), se.Err)
	}
	if calls := f.calls(); len(calls) != 1 || !calls[0].initial {
		t.Fatalf("calls = %+v, want only the initial exec", calls)
	}
	t.Logf("no attach after %s with a nominal %s minimum backoff", time.Since(start), c.timing.backoffMin)
}

func execRetryStdout(offset int64, data string) *sandrunv1.DevSessionExecEvent {
	return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
		Stdout: &sandrunv1.DevSessionExecOutput{Offset: offset, Data: []byte(data)},
	}}
}

func TestExecRetryProgressResetsBackoff(t *testing.T) {
	for _, tc := range []struct {
		name        string
		firstStart  bool
		event       *sandrunv1.DevSessionExecEvent
		wantOffset  int64
		wantBackoff time.Duration
	}{
		{name: "first start", firstStart: true, event: retryStarted, wantOffset: 3, wantBackoff: 25 * time.Millisecond},
		{name: "new output", event: execRetryStdout(1, "bcde"), wantOffset: 5, wantBackoff: 25 * time.Millisecond},
		{name: "truncation", event: &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{
			OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{StdoutOffset: 8},
		}}, wantOffset: 8, wantBackoff: 25 * time.Millisecond},
		{name: "replayed output", event: execRetryStdout(1, "bc"), wantOffset: 3, wantBackoff: 100 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newExecRetryFixture(t, func(_ context.Context, index int, send func(*sandrunv1.DevSessionExecEvent) error) error {
				switch index {
				case 0:
					if !tc.firstStart {
						if err := send(retryStarted); err != nil {
							return err
						}
					}
					if err := send(execRetryStdout(0, "abc")); err != nil {
						return err
					}
				case 2:
					if err := send(tc.event); err != nil {
						return err
					}
				case 3:
					return send(retryExited)
				}
				return execRetryUnavailable()
			})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			var events collected
			res, err := c.Exec(ctx, ExecParams{ExecID: "exec", Command: []string{"true"}, OnEvent: events.on})
			if err != nil || res.Kind != ExecResultExited || res.ExitCode == nil || *res.ExitCode != 0 ||
				res.StdoutOffset != tc.wantOffset || events.started != 1 {
				t.Fatalf("result = %+v, error = %v, starts = %d", res, err, events.started)
			}
			want := []execRetryCall{
				{initial: true, id: "exec"}, {id: "exec", stdout: 3}, {id: "exec", stdout: 3},
				{id: "exec", stdout: tc.wantOffset},
			}
			calls := f.calls()
			if len(calls) != len(want) {
				t.Fatalf("calls = %+v, want %+v", calls, want)
			}
			for i, call := range calls {
				if call != want[i] {
					t.Fatalf("call %d = %+v, want %+v", i, call, want[i])
				}
			}
			delays := f.delays(t)
			t.Logf("planned delays = %v, final stdout offset = %d", delays, res.StdoutOffset)
			if len(delays) != 3 {
				t.Fatalf("delays = %v, want three", delays)
			}
			requireExecRetryDelay(t, delays[0], 25*time.Millisecond)
			requireExecRetryDelay(t, delays[1], 50*time.Millisecond)
			requireExecRetryDelay(t, delays[2], tc.wantBackoff)
		})
	}
}

func TestExecRetryHealthyIdleRotation(t *testing.T) {
	budget := 160 * time.Millisecond
	for _, tc := range []struct {
		name          string
		lateKeepalive bool
		wantDelays    []time.Duration
	}{
		{name: "idle after keepalive", wantDelays: []time.Duration{25 * time.Millisecond}},
		{name: "late first keepalive", lateKeepalive: true, wantDelays: []time.Duration{25 * time.Millisecond, 50 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, f := newExecRetryFixture(t, func(ctx context.Context, index int, send func(*sandrunv1.DevSessionExecEvent) error) error {
				switch index {
				case 0:
					return send(retryStarted)
				case 1:
					if !tc.lateKeepalive {
						if err := send(retryKeepalive); err != nil {
							return err
						}
					}
					if err := sleep(ctx, budget+100*time.Millisecond); err != nil {
						return err
					}
					return send(retryKeepalive)
				case 2:
					return execRetryUnavailable()
				default:
					return send(retryExited)
				}
			})
			c.timing.retryBudget = budget
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			res, err := c.Exec(ctx, ExecParams{ExecID: "exec", Command: []string{"true"}})
			if err != nil || res.Kind != ExecResultExited {
				t.Fatalf("result = %+v, error = %v", res, err)
			}
			delays := f.delays(t)
			t.Logf("planned delays = %v", delays)
			if len(delays) != len(tc.wantDelays) {
				t.Fatalf("delays = %v, want %v before jitter", delays, tc.wantDelays)
			}
			for i, want := range tc.wantDelays {
				requireExecRetryDelay(t, delays[i], want)
			}
		})
	}
}

func TestExecRetryProgressLeavesStreamOpen(t *testing.T) {
	budget := 160 * time.Millisecond
	c, f := newExecRetryFixture(t, func(ctx context.Context, index int, send func(*sandrunv1.DevSessionExecEvent) error) error {
		if index == 0 {
			return execRetryUnavailable()
		}
		if index > 1 {
			return connect.NewError(connect.CodeInternal, errors.New("unexpected extra attach"))
		}
		if err := send(retryStarted); err != nil {
			return err
		}
		if err := sleep(ctx, budget+100*time.Millisecond); err != nil {
			return err
		}
		return send(retryExited)
	})
	c.timing.retryBudget = budget
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	res, err := c.Exec(ctx, ExecParams{ExecID: "exec", Command: []string{"true"}})
	if err != nil || res.Kind != ExecResultExited || len(f.calls()) != 2 {
		t.Fatalf("result = %+v, error = %v, calls = %+v", res, err, f.calls())
	}
}

func TestExecInitialErrorAfterKeepalive(t *testing.T) {
	for _, keepalive := range []bool{false, true} {
		name := "before any frame"
		if keepalive {
			name = "after keepalive"
		}
		t.Run(name, func(t *testing.T) {
			c, f := newExecRetryFixture(t, func(_ context.Context, _ int, send func(*sandrunv1.DevSessionExecEvent) error) error {
				if keepalive {
					if err := send(retryKeepalive); err != nil {
						return err
					}
				}
				return connect.NewError(connect.CodePermissionDenied, errors.New("exec denied"))
			})
			_, err := c.Exec(t.Context(), ExecParams{ExecID: "exec", Command: []string{"true"}})
			var se *ExecStreamError
			if connect.CodeOf(err) != connect.CodePermissionDenied || errors.As(err, &se) != keepalive {
				t.Fatalf("error = %v, want PermissionDenied with ExecStreamError=%t", err, keepalive)
			}
			if calls := f.calls(); len(calls) != 1 || !calls[0].initial {
				t.Fatalf("calls = %+v, want only the initial exec", calls)
			}
		})
	}
}
