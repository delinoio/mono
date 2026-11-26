// SPDX-License-Identifier: Apache-2.0

package sandrun

import (
	"bytes"
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

// needPOSIX skips tests whose fake guest runs sh and tar locally.
func needPOSIX(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
}

// newSession creates a running dev session of a fresh profile whose
// workspace is a new local directory.
func newSession(t *testing.T, c *Client, srv *sandruntest.Server, org string, os sandrunv1.GuestOs) *sandrunv1.DevSession {
	t.Helper()
	ws := filepath.Join(t.TempDir(), "work")
	if err := mkdir(ws); err != nil {
		t.Fatal(err)
	}
	prof := srv.AddDevSessionProfile(org, "p-"+strings.ToLower(os.String()), os, ws)
	res, err := c.DevSessions().CreateDevSession(context.Background(), connect.NewRequest(&sandrunv1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId()}))
	if err != nil {
		t.Fatal(err)
	}
	return res.Msg.GetDevSession()
}

type collected struct {
	stdout, stderr bytes.Buffer
	started        int
	truncated      []ExecEvent
}

func (c *collected) on(ev ExecEvent) error {
	switch ev.Kind {
	case ExecEventStarted:
		c.started++
	case ExecEventStdout:
		c.stdout.Write(ev.Data)
	case ExecEventStderr:
		c.stderr.Write(ev.Data)
	case ExecEventOutputTruncated:
		c.truncated = append(c.truncated, ev)
	}
	return nil
}

func TestExecCallbackResume(t *testing.T) {
	needPOSIX(t)
	rejected := errors.New("output rejected")
	for _, tc := range []struct {
		kind           ExecEventKind
		command        string
		stdout, stderr string
	}{
		{ExecEventStdout, "printf lost", "lost", ""},
		{ExecEventStderr, "printf lost >&2", "", "lost"},
	} {
		t.Run(tc.kind.String(), func(t *testing.T) {
			c, srv, org := newTestClient(t, "")
			ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
			ctx := context.Background()
			_, err := c.Exec(ctx, ExecParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
				Command: []string{"sh", "-c", tc.command},
				OnEvent: func(ev ExecEvent) error {
					if ev.Kind == tc.kind {
						return rejected
					}
					return nil
				},
			})
			se := requireExecCallbackError(t, err, rejected, 0, 0)

			var got collected
			res, err := c.AttachExec(ctx, AttachParams{
				OrganizationID: org, DevSessionID: ds.GetDevSessionId(), ExecID: se.ExecID,
				StdoutOffset: se.StdoutOffset, StderrOffset: se.StderrOffset, OnEvent: got.on,
			})
			if err != nil || res.Kind != ExecResultExited || res.ExitCode == nil || *res.ExitCode != 0 || res.Incomplete {
				t.Fatalf("attach result = %+v, error = %v", res, err)
			}
			if got.stdout.String() != tc.stdout || got.stderr.String() != tc.stderr {
				t.Fatalf("replayed stdout = %q, stderr = %q", got.stdout.String(), got.stderr.String())
			}
			if n := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure); n != 1 {
				t.Fatalf("exec calls = %d, want 1", n)
			}
		})
	}
}

type execAttachScript struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	events func(*sandrunv1.AttachDevSessionExecRequest) []*sandrunv1.DevSessionExecEvent
}

func (s execAttachScript) AttachDevSessionExec(_ context.Context, req *connect.Request[sandrunv1.AttachDevSessionExecRequest], stream *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error {
	for _, ev := range s.events(req.Msg) {
		if err := stream.Send(&sandrunv1.AttachDevSessionExecResponse{Event: ev}); err != nil {
			return err
		}
	}
	return nil
}

func TestExecCallbackResumeAttach(t *testing.T) {
	rejected := errors.New("output rejected")
	stdout := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
		Stdout: &sandrunv1.DevSessionExecOutput{Offset: 1, Data: []byte("bcdef")},
	}}
	gap := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
		Stdout: &sandrunv1.DevSessionExecOutput{Offset: 5, Data: []byte("f")},
	}}
	stderr := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stderr{
		Stderr: &sandrunv1.DevSessionExecOutput{Offset: 5, Data: []byte("f")},
	}}
	truncated := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{
		OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{StdoutOffset: 3, StderrOffset: 5},
	}}
	exited := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{
		Exited: &sandrunv1.DevSessionExecExited{Status: &sandrunv1.DevSessionExecExited_ExitCode{}},
	}}

	t.Run("rejected output", func(t *testing.T) {
		for _, tc := range []struct {
			name                     string
			events                   []*sandrunv1.DevSessionExecEvent
			kind                     ExecEventKind
			data                     string
			stdout, stderr           int64
			finalStdout, finalStderr int64
			missingOut, missingErr   ByteRange
		}{
			{
				name: "trimmed stdout", events: []*sandrunv1.DevSessionExecEvent{stdout, exited},
				kind: ExecEventStdout, data: "def", stdout: 3, stderr: 2, finalStdout: 6, finalStderr: 2,
			},
			{
				name: "stdout after gap", events: []*sandrunv1.DevSessionExecEvent{gap, exited},
				kind: ExecEventStdout, data: "f", stdout: 5, stderr: 2, finalStdout: 6, finalStderr: 2,
				missingOut: ByteRange{From: 3, To: 5},
			},
			{
				name: "stderr after truncation", events: []*sandrunv1.DevSessionExecEvent{truncated, stderr, exited},
				kind: ExecEventStderr, data: "f", stdout: 3, stderr: 5, finalStdout: 3, finalStderr: 6,
				missingErr: ByteRange{From: 2, To: 5},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c := newExecAttachClient(t, func(*sandrunv1.AttachDevSessionExecRequest) []*sandrunv1.DevSessionExecEvent {
					return tc.events
				})
				var got collected
				var offered ExecEvent
				params := AttachParams{ExecID: "exec", StdoutOffset: 3, StderrOffset: 2, OnEvent: func(ev ExecEvent) error {
					if ev.Kind == tc.kind {
						offered = ev
						return rejected
					}
					return got.on(ev)
				}}
				_, err := c.AttachExec(context.Background(), params)
				se := requireExecCallbackError(t, err, rejected, tc.stdout, tc.stderr)
				offset := tc.stdout
				if tc.kind == ExecEventStderr {
					offset = tc.stderr
				}
				if offered.ExecID != params.ExecID || offered.Offset != offset || string(offered.Data) != tc.data {
					t.Fatalf("rejected event = %+v, data = %q", offered, offered.Data)
				}
				if tc.missingOut.Empty() && tc.missingErr.Empty() {
					if len(got.truncated) != 0 {
						t.Fatalf("unexpected truncation: %+v", got.truncated)
					}
				} else if len(got.truncated) != 1 || got.truncated[0].MissingStdout != tc.missingOut || got.truncated[0].MissingStderr != tc.missingErr {
					t.Fatalf("truncation = %+v, want stdout %+v and stderr %+v", got.truncated, tc.missingOut, tc.missingErr)
				}

				got = collected{}
				res, err := c.AttachExec(context.Background(), AttachParams{
					ExecID: se.ExecID, StdoutOffset: se.StdoutOffset, StderrOffset: se.StderrOffset, OnEvent: got.on,
				})
				if err != nil || res.StdoutOffset != tc.finalStdout || res.StderrOffset != tc.finalStderr || res.Incomplete {
					t.Fatalf("reattach result = %+v, error = %v", res, err)
				}
				out := got.stdout.String()
				if tc.kind == ExecEventStderr {
					out = got.stderr.String()
				}
				if out != tc.data || len(got.truncated) != 0 {
					t.Fatalf("replayed output = %q, truncation = %+v", out, got.truncated)
				}
			})
		}
	})

	t.Run("accepted output", func(t *testing.T) {
		for _, tc := range []struct {
			name     string
			callback bool
			want     string
		}{
			{"callback", true, "def"},
			{"nil callback", false, ""},
		} {
			t.Run(tc.name, func(t *testing.T) {
				var got collected
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				c := newExecAttachClient(t, func(req *sandrunv1.AttachDevSessionExecRequest) []*sandrunv1.DevSessionExecEvent {
					if req.GetStdoutOffset() == 6 {
						return []*sandrunv1.DevSessionExecEvent{stdout, exited}
					}
					// Rotate the first stream before sending the final result.
					return []*sandrunv1.DevSessionExecEvent{stdout}
				})
				params := AttachParams{ExecID: "exec", StdoutOffset: 3, StderrOffset: 2}
				if tc.callback {
					params.OnEvent = got.on
				}
				res, err := c.AttachExec(ctx, params)
				if err != nil || res.StdoutOffset != 6 || res.StderrOffset != 2 || got.stdout.String() != tc.want {
					t.Fatalf("attach result = %+v, error = %v, stdout = %q", res, err, got.stdout.String())
				}
			})
		}
	})
}

func TestExecTruncationCallbackResume(t *testing.T) {
	rejected := errors.New("truncation rejected")
	explicit := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{
		OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{StdoutOffset: 7, StderrOffset: 9},
	}}
	gap := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
		Stdout: &sandrunv1.DevSessionExecOutput{Offset: 7, Data: []byte("end")},
	}}
	exited := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{
		Exited: &sandrunv1.DevSessionExecExited{Status: &sandrunv1.DevSessionExecExited_ExitCode{}},
	}}

	for _, tc := range []struct {
		name                     string
		finalStdout, finalStderr int64
		missingStderr            ByteRange
		stdout                   string
	}{
		{name: "explicit notice", finalStdout: 7, finalStderr: 9, missingStderr: ByteRange{From: 2, To: 9}},
		{name: "inferred stdout gap", finalStdout: 10, finalStderr: 2, stdout: "end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newExecAttachClient(t, func(req *sandrunv1.AttachDevSessionExecRequest) []*sandrunv1.DevSessionExecEvent {
				if tc.missingStderr.Empty() {
					return []*sandrunv1.DevSessionExecEvent{gap, exited}
				}
				if req.GetStdoutOffset() < 7 || req.GetStderrOffset() < 9 {
					return []*sandrunv1.DevSessionExecEvent{explicit, exited}
				}
				return []*sandrunv1.DevSessionExecEvent{exited}
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			params := AttachParams{ExecID: "exec", StdoutOffset: 3, StderrOffset: 2}
			params.OnEvent = func(ev ExecEvent) error {
				if ev.Kind == ExecEventOutputTruncated {
					return rejected
				}
				t.Fatalf("unexpected event before truncation: %+v", ev)
				return nil
			}
			_, err := c.AttachExec(ctx, params)
			se := requireExecCallbackError(t, err, rejected, 3, 2)
			if se.Err != rejected {
				t.Fatalf("stream error cause = %v, want the original callback error", se.Err)
			}

			var got collected
			params.ExecID, params.StdoutOffset, params.StderrOffset = se.ExecID, se.StdoutOffset, se.StderrOffset
			params.OnEvent = got.on
			res, err := c.AttachExec(ctx, params)
			if err != nil || res.Kind != ExecResultExited || !res.Incomplete ||
				res.StdoutOffset != tc.finalStdout || res.StderrOffset != tc.finalStderr {
				t.Fatalf("reattach result = %+v, error = %v", res, err)
			}
			wantStdout := ByteRange{From: 3, To: 7}
			if len(got.truncated) != 1 || got.truncated[0].MissingStdout != wantStdout ||
				got.truncated[0].MissingStderr != tc.missingStderr || got.stdout.String() != tc.stdout {
				t.Fatalf("replayed truncation = %+v, stdout = %q, want missing stdout %+v, missing stderr %+v and stdout %q",
					got.truncated, got.stdout.String(), wantStdout, tc.missingStderr, tc.stdout)
			}

			params.OnEvent = nil
			res, err = c.AttachExec(ctx, params)
			if err != nil || res.Kind != ExecResultExited || !res.Incomplete ||
				res.StdoutOffset != tc.finalStdout || res.StderrOffset != tc.finalStderr {
				t.Fatalf("attach with nil callback result = %+v, error = %v", res, err)
			}
		})
	}
}

func TestExecInferredGapCancellation(t *testing.T) {
	const execID = "exec"
	rejected := errors.New("gap rejected")
	started := ExecEvent{Kind: ExecEventStarted, ExecID: execID}
	output := func(kind ExecEventKind, offset int64, data string) *sandrunv1.DevSessionExecEvent {
		chunk := &sandrunv1.DevSessionExecOutput{Offset: offset, Data: []byte(data)}
		if kind == ExecEventStdout {
			return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{Stdout: chunk}}
		}
		return &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stderr{Stderr: chunk}}
	}
	for _, kind := range []ExecEventKind{ExecEventStdout, ExecEventStderr} {
		gap := ExecEvent{Kind: ExecEventOutputTruncated, ExecID: execID}
		gapCall := execRetryCall{id: execID, stdout: 1, stderr: 2}
		finalStdout, finalStderr := int64(1), int64(2)
		if kind == ExecEventStdout {
			gap.MissingStdout = ByteRange{From: 1, To: 4}
			gapCall.stdout, finalStdout = 4, 7
		} else {
			gap.MissingStderr = ByteRange{From: 2, To: 4}
			gapCall.stderr, finalStderr = 4, 7
		}
		chunk := ExecEvent{Kind: kind, ExecID: execID, Offset: 4, Data: []byte("end")}
		for _, method := range []struct {
			name    string
			initial bool
		}{
			{name: "Exec", initial: true},
			{name: "AttachExec"},
		} {
			for _, tc := range []struct {
				name string
				err  error
			}{
				{name: "cancel", err: context.Canceled},
				{name: "active"},
				{name: "reject", err: rejected},
			} {
				t.Run(kind.String()+"/"+method.name+"/"+tc.name, func(t *testing.T) {
					c, fixture := newExecRetryFixture(t, func(_ context.Context, call int, send func(*sandrunv1.DevSessionExecEvent) error) error {
						events := []*sandrunv1.DevSessionExecEvent{retryStarted}
						if method.initial && call == 0 {
							events = append(events, output(ExecEventStdout, 0, "a"), output(ExecEventStderr, 0, "bc"))
						}
						events = append(events, output(kind, 4, "end"), retryExited)
						for _, ev := range events {
							if err := send(ev); err != nil {
								return err
							}
						}
						return nil
					})
					ctx, cancel := context.WithCancel(t.Context())
					defer cancel()
					var got, want []ExecEvent
					onEvent := func(ev ExecEvent) error {
						got = append(got, ev)
						if ev.Kind == ExecEventOutputTruncated && tc.err != nil {
							cancel()
							if tc.err == rejected {
								return rejected
							}
						}
						return nil
					}
					params := AttachParams{ExecID: execID, StdoutOffset: 1, StderrOffset: 2, OnEvent: onEvent}
					firstCall := execRetryCall{id: execID, stdout: 1, stderr: 2}
					var res *ExecResult
					var err error
					if method.initial {
						firstCall = execRetryCall{initial: true, id: execID}
						want = append(want, started,
							ExecEvent{Kind: ExecEventStdout, ExecID: execID, Data: []byte("a")},
							ExecEvent{Kind: ExecEventStderr, ExecID: execID, Data: []byte("bc")})
						res, err = c.Exec(ctx, ExecParams{ExecID: execID, Command: []string{"fixture"}, OnEvent: onEvent})
					} else {
						res, err = c.AttachExec(ctx, params)
					}
					want = append(want, gap)
					if tc.err == nil {
						want = append(want, chunk)
					}
					if !reflect.DeepEqual(got, want) {
						t.Errorf("events = %+v, want %+v", got, want)
					}
					if calls := fixture.calls(); !slices.Equal(calls, []execRetryCall{firstCall}) {
						t.Errorf("calls = %+v, want only %+v", calls, firstCall)
					}
					if tc.err == nil {
						if err != nil || res == nil || res.Kind != ExecResultExited || !res.Incomplete ||
							res.StdoutOffset != finalStdout || res.StderrOffset != finalStderr {
							t.Fatalf("active result = %+v, error = %v", res, err)
						}
						return
					}
					if res != nil {
						t.Fatalf("result = %+v, want nil", res)
					}
					if tc.err == rejected {
						se := requireExecRetryError(t, err, execID, 1, 2, true)
						if se.Err != rejected {
							t.Fatalf("error cause = %v, want original rejection", se.Err)
						}
						return
					}
					se := requireExecRetryError(t, err, execID, gapCall.stdout, gapCall.stderr, true)
					if !errors.Is(se, context.Canceled) {
						t.Fatalf("error = %v, want wrapped context.Canceled", se)
					}
					got = nil
					params.ExecID, params.StdoutOffset, params.StderrOffset = se.ExecID, se.StdoutOffset, se.StderrOffset
					res, err = c.AttachExec(t.Context(), params)
					if err != nil || res == nil || res.Kind != ExecResultExited || res.Incomplete ||
						res.StdoutOffset != finalStdout || res.StderrOffset != finalStderr ||
						!reflect.DeepEqual(got, []ExecEvent{chunk}) {
						t.Fatalf("reattach result = %+v, error = %v, events = %+v, want one replay of %+v", res, err, got, chunk)
					}
					if calls := fixture.calls(); !slices.Equal(calls, []execRetryCall{firstCall, gapCall}) {
						t.Fatalf("calls = %+v, want %+v followed by %+v", calls, firstCall, gapCall)
					}
				})
			}
		}
	}
}

func newExecAttachClient(t *testing.T, events func(*sandrunv1.AttachDevSessionExecRequest) []*sandrunv1.DevSessionExecEvent) *Client {
	t.Helper()
	_, handler := sandrunv1connect.NewDevSessionServiceHandler(execAttachScript{events: events})
	srv := httptest.NewTLSServer(handler)
	t.Cleanup(srv.Close)
	c, err := New(Options{BaseURL: srv.URL, HTTPClient: srv.Client(), APIKey: sandruntest.DefaultAPIKey})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func requireExecCallbackError(t *testing.T, err, rejected error, stdout, stderr int64) *ExecStreamError {
	t.Helper()
	var se *ExecStreamError
	if !errors.As(err, &se) || !errors.Is(err, rejected) {
		t.Fatalf("error = %v, want ExecStreamError wrapping %v", err, rejected)
	}
	if se.ExecID == "" || se.StdoutOffset != stdout || se.StderrOffset != stderr {
		t.Fatalf("stream error has exec ID %q and offsets (%d, %d), want exec ID and offsets (%d, %d)",
			se.ExecID, se.StdoutOffset, se.StderrOffset, stdout, stderr)
	}
	return se
}

func TestExecAttachAfterStreamRotation(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	srv.ExecStreamEventLimit = 2
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got collected
	res, err := c.Exec(ctx, ExecParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		Command: []string{"sh", "-c", `for i in 1 2 3 4 5 6; do echo "out $i"; echo "err $i" >&2; sleep 0.01; done; exit 3`},
		OnEvent: got.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ExecResultExited || res.ExitCode == nil || *res.ExitCode != 3 || res.Incomplete {
		t.Fatalf("result = %+v", res)
	}
	wantOut := "out 1\nout 2\nout 3\nout 4\nout 5\nout 6\n"
	wantErr := "err 1\nerr 2\nerr 3\nerr 4\nerr 5\nerr 6\n"
	if got.stdout.String() != wantOut || got.stderr.String() != wantErr || got.started != 1 {
		t.Fatalf("stdout %q stderr %q started %d", got.stdout.String(), got.stderr.String(), got.started)
	}
	if res.StdoutOffset != int64(len(wantOut)) || res.StderrOffset != int64(len(wantErr)) {
		t.Fatalf("stdout offset %d, stderr offset %d", res.StdoutOffset, res.StderrOffset)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); n < 2 {
		t.Fatalf("attach calls = %d, want several", n)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure); n != 1 {
		t.Fatalf("exec calls = %d, want 1", n)
	}
	reqs := srv.ExecRequests()
	if len(reqs) != 1 || len(reqs[0].GetExecId()) != 36 {
		t.Fatalf("exec requests %v", reqs)
	}
}

func TestExecNotFoundAfterStreamRotation(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	// End the first stream immediately after Started, then reject the attach.
	srv.ExecStreamEventLimit = 1
	srv.Fail(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure, connect.NewError(connect.CodeNotFound, errors.New("exec not found")))

	var got collected
	_, err := c.Exec(t.Context(), ExecParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		Command: []string{"true"}, OnEvent: got.on,
	})
	var se *ExecStreamError
	if !errors.As(err, &se) || connect.CodeOf(se.Err) != connect.CodeNotFound {
		t.Fatalf("exec error = %v, want ExecStreamError wrapping not_found", err)
	}
	if !se.Started || se.OutcomeUnknown || got.started != 1 {
		t.Fatalf("stream error = %+v, started = %t, outcome unknown = %t, start callbacks = %d", se, se.Started, se.OutcomeUnknown, got.started)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure); n != 1 {
		t.Fatalf("exec calls = %d, want 1", n)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); n != 1 {
		t.Fatalf("attach calls = %d, want 1", n)
	}
}

func TestExecReportsLostOutput(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	srv.ExecStreamEventLimit = 2
	srv.OutputKeepBytes = 8
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var got collected
	res, err := c.Exec(ctx, ExecParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		Command: []string{"sh", "-c", `echo first; sleep 0.2; echo 0123456789abcdefghij; sleep 0.2; echo end`},
		OnEvent: got.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Kind != ExecResultExited || res.ExitCode == nil || *res.ExitCode != 0 || !res.Incomplete || len(got.truncated) == 0 {
		t.Fatalf("result = %+v, truncation = %v", res, got.truncated)
	}
	tr := got.truncated[0].MissingStdout
	if tr.Empty() || tr.From < int64(len("first\n")) {
		t.Fatalf("missing range %+v", tr)
	}
	if !strings.HasSuffix(got.stdout.String(), "end\n") {
		t.Fatalf("stdout %q", got.stdout.String())
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); n == 0 {
		t.Fatal("expected an attach after rotation")
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure); n != 1 {
		t.Fatalf("exec calls = %d, want 1", n)
	}
}

func TestExecTimeoutValidation(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	params := ExecParams{
		OrganizationID: org,
		DevSessionID:   ds.GetDevSessionId(),
		Command:        []string{"true"},
	}

	t.Run("negative", func(t *testing.T) {
		p := params
		p.Timeout = -time.Nanosecond
		callbacks := 0
		p.OnEvent = func(ExecEvent) error {
			callbacks++
			return nil
		}
		res, err := c.Exec(t.Context(), p)
		if res != nil || connect.CodeOf(err) != connect.CodeInvalidArgument || connect.IsWireError(err) {
			t.Errorf("Exec returned (%+v, %v), want a nil result and a local invalid_argument error", res, err)
		}
		if calls := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure); calls != 0 {
			t.Errorf("start calls = %d, want 0", calls)
		}
		if calls := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure); calls != 0 {
			t.Errorf("attach calls = %d, want 0", calls)
		}
		if callbacks != 0 {
			t.Errorf("callback calls = %d, want 0", callbacks)
		}
	})

	for _, tc := range []struct {
		name    string
		timeout time.Duration
	}{
		{name: "zero", timeout: 0},
		{name: "positive", timeout: time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := params
			p.Timeout = tc.timeout
			before := len(srv.ExecRequests())
			if _, err := c.Exec(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			requests := srv.ExecRequests()
			if got := len(requests) - before; got != 1 {
				t.Fatalf("start requests = %d, want 1", got)
			}
			timeout := requests[before].GetTimeout()
			if tc.timeout == 0 {
				if timeout != nil {
					t.Fatalf("timeout = %v, want an absent field", timeout)
				}
			} else if timeout == nil || timeout.AsDuration() != tc.timeout {
				t.Fatalf("timeout = %v, want %s", timeout, tc.timeout)
			}
		})
	}
}

func TestExecErrors(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	ctx := context.Background()

	res, err := c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"sandrun-no-such-program"}})
	if err != nil || res.Kind != ExecResultFailed || res.FailureReason != sandrunv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("missing program: %+v %v", res, err)
	}
	res, err = c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond})
	if err != nil || !res.TimedOut {
		t.Fatalf("timeout: %+v %v", res, err)
	}

	// A server error of ExecDevSession is returned with its code.
	srv.SetSessionState(ds.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_QUEUED)
	_, err = c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"true"}})
	var se *ExecStreamError
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || errors.As(err, &se) {
		t.Fatalf("not running: %v", err)
	}
	srv.SetSessionState(ds.GetDevSessionId(), sandrunv1.RequestState_REQUEST_STATE_RUNNING)

	// An unknown exec on attach reports an unknown outcome.
	var got collected
	_, err = c.AttachExec(ctx, AttachParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(), ExecID: "missing",
		StdoutOffset: 10, StderrOffset: 30, OnEvent: got.on,
	})
	if !errors.As(err, &se) || connect.CodeOf(se.Err) != connect.CodeNotFound {
		t.Fatalf("attach error = %v, want ExecStreamError wrapping not_found", err)
	}
	if se.ExecID != "missing" || se.StdoutOffset != 10 || se.StderrOffset != 30 {
		t.Fatalf("attach error = %+v, want exec ID missing and offsets (10, 30)", se)
	}
	if se.Started || !se.OutcomeUnknown || got.started != 0 {
		t.Fatalf("started = %t, outcome unknown = %t, start callbacks = %d", se.Started, se.OutcomeUnknown, got.started)
	}

	// A cancelled caller gets the offsets to continue with.
	cctx, cancel := context.WithCancel(ctx)
	_, err = c.Exec(cctx, ExecParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		Command: []string{"sh", "-c", "echo hello; sleep 0.5; echo world"},
		OnEvent: func(ev ExecEvent) error {
			_ = got.on(ev)
			if ev.Kind == ExecEventStdout {
				cancel()
			}
			return nil
		},
	})
	if !errors.As(err, &se) || !se.Started || se.StdoutOffset != 6 {
		t.Fatalf("cancelled exec: %#v", err)
	}
	got = collected{}
	res, err = c.AttachExec(ctx, AttachParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), ExecID: se.ExecID, StdoutOffset: se.StdoutOffset, OnEvent: got.on})
	if err != nil || got.stdout.String() != "world\n" || got.started != 0 || res.ExitCode == nil || *res.ExitCode != 0 {
		t.Fatalf("attach after cancel: %q %+v %v", got.stdout.String(), res, err)
	}

	// After rotation, a transient failure of the first attach is retried
	// without running the command again.
	execCalls := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure)
	attachCalls := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure)
	srv.Fail(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure, connect.NewError(connect.CodeUnavailable, errors.New("restart")))
	srv.ExecStreamEventLimit = 2
	retryCtx, cancelRetry := context.WithTimeout(ctx, 5*time.Second)
	defer cancelRetry()
	got = collected{}
	res, err = c.Exec(retryCtx, ExecParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		Command: []string{"sh", "-c", "echo a; echo b; echo retry >&2"},
		OnEvent: got.on,
	})
	if err != nil || res.Kind != ExecResultExited || res.ExitCode == nil || *res.ExitCode != 0 || res.Incomplete {
		t.Fatalf("retry result = %+v, error = %v", res, err)
	}
	if got.stdout.String() != "a\nb\n" || got.stderr.String() != "retry\n" {
		t.Fatalf("retry stdout = %q, stderr = %q", got.stdout.String(), got.stderr.String())
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceAttachDevSessionExecProcedure) - attachCalls; n < 2 {
		t.Fatalf("attach calls = %d, want at least 2", n)
	}
	if n := srv.Calls(sandrunv1connect.DevSessionServiceExecDevSessionProcedure) - execCalls; n != 1 {
		t.Fatalf("exec calls = %d, want 1", n)
	}
}

func TestFakeExecCloseStopsGroup(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("the fake guest stops exec process groups on Linux and macOS")
	}
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, sandrunv1.GuestOs_GUEST_OS_LINUX)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	// Redirect the child's descriptors so closing the exec stream cannot
	// affect whether the child survives. Bound its lifetime on failure.
	const command = `
sh -c '
	: > ready
	i=0
	while [ "$i" -lt 200 ]; do
		if [ -e release ]; then
			: > survived
			exit
		fi
		i=$((i + 1))
		sleep 0.025
	done
' </dev/null >/dev/null 2>&1 &
wait
`
	done := make(chan error, 1)
	go func() {
		_, err := c.Exec(ctx, ExecParams{
			OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
			Command: []string{"sh", "-c", command},
		})
		done <- err
	}()
	workspace := ds.GetWorkspacePath()
	ready := filepath.Join(workspace, "ready")
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		_, err := os.Stat(ready)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		select {
		case err := <-done:
			t.Fatalf("exec finished before the child was ready: %v", err)
		case <-ctx.Done():
			t.Fatal("child did not become ready")
		case <-tick.C:
		}
	}

	closed := make(chan struct{})
	go func() {
		srv.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-ctx.Done():
		t.Fatal("server did not close")
	}
	if err := os.WriteFile(filepath.Join(workspace, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("exec did not finish after Close: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("exec did not finish after Close")
	}
	select {
	case <-time.After(500 * time.Millisecond):
	case <-ctx.Done():
		t.Fatal("deadline expired while checking for the survival marker")
	}
	if _, err := os.Stat(filepath.Join(workspace, "survived")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("survival marker after Close: stat returned %v, want not exist", err)
	}
}

func mkdir(p string) error { return os.MkdirAll(p, 0o755) }
