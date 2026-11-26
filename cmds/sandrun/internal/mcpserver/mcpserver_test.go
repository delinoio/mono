// SPDX-License-Identifier: Apache-2.0

package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	"github.com/delinoio/mono/go/sandrun"
	"github.com/delinoio/mono/go/sandrun/sandruntest"
	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
)

type harness struct {
	t       *testing.T
	srv     *sandruntest.Server
	org     string
	session *mcp.ClientSession
	server  *Server
	ws      string
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	srv := sandruntest.New()
	t.Cleanup(srv.Close)
	org := srv.AddOrganization("test").GetOrganizationId()
	ws := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.AddDevSessionProfile(org, "linux", sandrunv1.GuestOs_GUEST_OS_LINUX, ws)
	if cfg.Client == nil {
		cfg.Client = func(context.Context) (*sandrun.Client, error) {
			return sandrun.New(sandrun.Options{BaseURL: srv.URL, APIKey: srv.APIKey})
		}
	}
	cfg.OrganizationID = org
	if cfg.LookupEnv == nil {
		cfg.LookupEnv = func(string) (string, bool) { return "", false }
	}
	if cfg.WorkDir == "" {
		cfg.WorkDir = t.TempDir()
	}
	s := New(cfg)
	t.Cleanup(s.Close)
	st, ct := mcp.NewInMemoryTransports()
	ctx := context.Background()
	if _, err := s.Connect(ctx, st); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &harness{t: t, srv: srv, org: org, session: cs, server: s, ws: ws}
}

// call calls a tool and decodes its structured result into out. It
// returns the text content and whether the result is an error.
func (h *harness) call(name string, args map[string]any, out any) (string, bool) {
	h.t.Helper()
	res, err := h.session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if out != nil && !res.IsError {
		reflect.ValueOf(out).Elem().SetZero()
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, out); err != nil {
			h.t.Fatalf("%s: decode %s: %v", name, b, err)
		}
	}
	return text.String(), res.IsError
}

func (h *harness) running() string {
	h.t.Helper()
	var r sessionResult
	if text, isErr := h.call("create_session", map[string]any{"profile": "linux"}, &r); isErr {
		h.t.Fatalf("create_session: %s", text)
	}
	return r.Session.SessionID
}

func needPOSIX(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake guest runs POSIX sh commands")
	}
}

func TestToolList(t *testing.T) {
	h := newHarness(t, Config{})
	res, err := h.session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	schemas := map[string]map[string]any{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		b, _ := json.Marshal(tool.InputSchema)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		schemas[tool.Name] = m
	}
	slices.Sort(names)
	want := []string{"create_session", "delete_session", "download", "exec", "exec_output", "get_session", "list_profiles", "list_sessions", "sync", "transfer_status", "wait_session"}
	if !slices.Equal(names, want) {
		t.Fatalf("tools = %v", names)
	}
	required := func(tool string) []string {
		var out []string
		for _, r := range schemas[tool]["required"].([]any) {
			out = append(out, r.(string))
		}
		slices.Sort(out)
		return out
	}
	for tool, want := range map[string][]string{
		"create_session": {"profile"}, "exec": {"command", "session_id"}, "exec_output": {"exec_id", "session_id", "stderr_offset", "stdout_offset"},
		"download": {"guest_path", "local_path", "session_id"}, "transfer_status": {"transfer_id"}, "sync": {"session_id"},
	} {
		if got := required(tool); !slices.Equal(got, want) {
			t.Errorf("%s required = %v, want %v", tool, got, want)
		}
	}
	props := schemas["exec"]["properties"].(map[string]any)
	for _, p := range []string{"cwd", "env", "timeout_seconds", "wait_seconds"} {
		if _, ok := props[p]; !ok {
			t.Errorf("exec has no %s argument", p)
		}
	}
}

func TestWaitLimit(t *testing.T) {
	h := newHarness(t, Config{})
	id := h.running()
	if text, isErr := h.call("wait_session", map[string]any{"session_id": id, "wait_seconds": 51}, nil); !isErr || !strings.HasPrefix(text, "invalid_argument") {
		t.Fatalf("wait_seconds 51 = %q %v", text, isErr)
	}
	if text, isErr := h.call("wait_session", map[string]any{"session_id": id, "wait_seconds": -1}, nil); !isErr {
		t.Fatalf("wait_seconds -1 accepted: %q", text)
	}
	var r sessionResult
	if text, isErr := h.call("wait_session", map[string]any{"session_id": id, "wait_seconds": 50}, &r); isErr || r.Session.State != "running" {
		t.Fatalf("wait_seconds 50 = %q %+v", text, r)
	}

	// A queued session returns its state when the wait passes.
	h.srv.NewSessionState = sandrunv1.RequestState_REQUEST_STATE_QUEUED
	queued := h.running()
	start := time.Now()
	if _, isErr := h.call("wait_session", map[string]any{"session_id": queued, "wait_seconds": 1}, &r); isErr || r.Session.State != "queued" {
		t.Fatalf("queued wait = %+v", r)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("the wait took longer than wait_seconds")
	}
	if _, isErr := h.call("wait_session", map[string]any{"session_id": queued, "wait_seconds": 0}, &r); isErr || r.Session.State != "queued" {
		t.Fatalf("zero wait = %+v", r)
	}
	if waitDur, err := h.server.waitDuration(nil); err != nil || waitDur != MaxWait {
		t.Fatalf("default wait = %v %v", waitDur, err)
	}
}

type toolDeadlineTransport func(*http.Request) (*http.Response, error)

func (f toolDeadlineTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestSessionToolDeadlines(t *testing.T) {
	const budget = 500 * time.Millisecond
	const profiles = sandrunv1connect.DevSessionServiceListDevSessionProfilesProcedure
	const sessions = sandrunv1connect.DevSessionServiceListDevSessionsProcedure

	type call struct {
		path string
		body proto.Message // A nil body stalls the RPC until its context ends.
	}
	for _, tc := range []struct {
		name  string
		args  map[string]any
		calls []call
	}{
		{name: "list_profiles", calls: []call{{path: profiles}}},
		{name: "create_session", args: map[string]any{"profile": "linux"}, calls: []call{{path: profiles}}},
		{name: "list_sessions", calls: []call{{path: sessions}}},
		{name: "get_session", args: map[string]any{"session_id": "test"}, calls: []call{{path: sandrunv1connect.DevSessionServiceGetDevSessionProcedure}}},
		{name: "delete_session", args: map[string]any{"session_id": "test"}, calls: []call{{path: sandrunv1connect.DevSessionServiceDeleteDevSessionProcedure}}},
		{name: "list_profiles", calls: []call{{profiles, &sandrunv1.ListDevSessionProfilesResponse{NextPageToken: "next"}}, {path: profiles}}},
		{name: "list_sessions", calls: []call{{sessions, &sandrunv1.ListDevSessionsResponse{NextPageToken: "next"}}, {path: sessions}}},
		{name: "create_session", args: map[string]any{"profile": "linux"}, calls: []call{
			{profiles, &sandrunv1.ListDevSessionProfilesResponse{NextPageToken: "next"}},
			{profiles, &sandrunv1.ListDevSessionProfilesResponse{Profiles: []*sandrunv1.DevSessionProfile{{Name: "linux", RunnerProfileId: "test-profile"}}}},
			{path: sandrunv1connect.DevSessionServiceCreateDevSessionProcedure},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var seen atomic.Int32
			var toolDeadline time.Time
			httpClient := &http.Client{Transport: toolDeadlineTransport(func(r *http.Request) (*http.Response, error) {
				i := int(seen.Add(1)) - 1
				if i >= len(tc.calls) {
					return nil, errors.New("unexpected RPC")
				}
				want := tc.calls[i]
				if r.URL.Path != want.path {
					t.Errorf("RPC %d path = %q, want %q", i, r.URL.Path, want.path)
				}
				deadline, ok := r.Context().Deadline()
				if !ok || time.Until(deadline) > budget {
					t.Errorf("RPC %d deadline = %v, want a deadline within %v", i, deadline, budget)
				}
				if i == 0 {
					toolDeadline = deadline
				} else if !deadline.Equal(toolDeadline) {
					t.Errorf("RPC %d deadline = %v, want the first RPC's deadline %v", i, deadline, toolDeadline)
				}
				if want.body == nil {
					<-r.Context().Done()
					return nil, r.Context().Err()
				}
				body, err := proto.Marshal(want.body)
				if err != nil {
					return nil, err
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": {"application/proto"}},
					Body:       io.NopCloser(bytes.NewReader(body)),
				}, nil
			})}
			client := func(context.Context) (*sandrun.Client, error) {
				return sandrun.New(sandrun.Options{BaseURL: "http://sandrun.test", APIKey: "test-key", HTTPClient: httpClient})
			}
			h := newHarness(t, Config{maxWait: budget, Client: client})
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			result, err := h.session.CallTool(ctx, &mcp.CallToolParams{Name: tc.name, Arguments: tc.args})
			if err != nil {
				t.Fatal(err)
			}
			if !result.IsError || len(result.Content) != 1 {
				t.Fatalf("tool result = %+v, want one error message", result)
			}
			if message, ok := result.Content[0].(*mcp.TextContent); !ok || !strings.HasPrefix(message.Text, "deadline_exceeded:") {
				t.Errorf("tool error = %v, want deadline_exceeded", result.Content[0])
			}
			if got := int(seen.Load()); got != len(tc.calls) {
				t.Errorf("RPCs = %d, want %d", got, len(tc.calls))
			}
		})
	}
}

func TestSessionToolDeadlineDuringClientSetup(t *testing.T) {
	for _, tc := range []struct {
		name string
		end  func(*testing.T, context.Context, <-chan struct{}) (context.Context, context.CancelFunc)
		code string
	}{
		{
			name: "earlier caller deadline",
			end: func(_ *testing.T, parent context.Context, _ <-chan struct{}) (context.Context, context.CancelFunc) {
				return context.WithTimeout(parent, 100*time.Millisecond)
			},
			code: "deadline_exceeded:",
		},
		{
			name: "caller cancellation",
			end: func(t *testing.T, parent context.Context, entered <-chan struct{}) (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(parent)
				go func() {
					select {
					case <-entered:
						cancel()
					case <-t.Context().Done():
					}
				}()
				return ctx, cancel
			},
			code: "canceled:",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			s := New(Config{maxWait: 3 * time.Second, Client: func(ctx context.Context) (*sandrun.Client, error) {
				defer close(finished)
				close(entered)
				<-release // Simulate a local file read that ignores cancellation.
				return nil, ctx.Err()
			}})
			t.Cleanup(s.Close)
			ctx, cancel := tc.end(t, t.Context(), entered)
			defer cancel()
			_, _, err := withToolDeadline(s.cfg.maxWait, s.getSession)(ctx, nil, sessionArgs{SessionID: "test"})
			close(release)
			<-finished
			if err == nil || !strings.HasPrefix(err.Error(), tc.code) || ctx.Err() == nil {
				t.Errorf("get_session = %v, caller context = %v, want %s", err, ctx.Err(), tc.code)
			}
		})
	}
}

func TestWaitSessionDeadline(t *testing.T) {
	for _, tc := range []struct {
		name          string
		waitSeconds   *int
		callerTimeout time.Duration
	}{
		{name: "default wait"},
		{name: "positive wait", waitSeconds: new(1)},
		{name: "zero wait", waitSeconds: new(0)},
		{name: "zero wait with earlier caller deadline", waitSeconds: new(0), callerTimeout: 200 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls atomic.Int32
			handler := connect.NewUnaryHandler(sandrunv1connect.DevSessionServiceGetDevSessionProcedure,
				func(ctx context.Context, _ *connect.Request[sandrunv1.GetDevSessionRequest]) (*connect.Response[sandrunv1.GetDevSessionResponse], error) {
					if calls.Add(1) != 1 {
						return nil, connect.NewError(connect.CodeInternal, errors.New("unexpected second lookup"))
					}
					<-ctx.Done()
					return nil, ctx.Err()
				})
			upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.callerTimeout > 0 {
					timeout := r.Header.Get("Connect-Timeout-Ms")
					ms, err := strconv.ParseInt(timeout, 10, 64)
					if err != nil || ms <= 0 || ms > tc.callerTimeout.Milliseconds() {
						t.Errorf("GetDevSession timeout = %q ms, want at most %v", timeout, tc.callerTimeout)
					}
				}
				// Stall until the client cancels the request.
				r.Header.Del("Connect-Timeout-Ms")
				handler.ServeHTTP(w, r)
			}))
			upstream.Config.Protocols = new(http.Protocols)
			upstream.Config.Protocols.SetUnencryptedHTTP2(true)
			upstream.Start()
			t.Cleanup(upstream.Close)
			h := newHarness(t, Config{maxWait: 500 * time.Millisecond, Client: func(context.Context) (*sandrun.Client, error) {
				return sandrun.New(sandrun.Options{BaseURL: upstream.URL, APIKey: "test-key"})
			}})
			if tc.callerTimeout > 0 {
				ctx, cancel := context.WithTimeout(t.Context(), tc.callerTimeout)
				defer cancel()
				_, _, err := h.server.waitSession(ctx, nil, waitSessionArgs{
					SessionID: "test-session", WaitSeconds: tc.waitSeconds,
				})
				if err == nil || !strings.HasPrefix(err.Error(), "deadline_exceeded:") || ctx.Err() != context.DeadlineExceeded {
					t.Errorf("wait_session = %v, caller context = %v", err, ctx.Err())
				}
			} else {
				// The outer MCP context stays live past the tool deadline.
				ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
				defer cancel()
				args := map[string]any{"session_id": "test-session"}
				if tc.waitSeconds != nil {
					args["wait_seconds"] = *tc.waitSeconds
				}
				result, err := h.session.CallTool(ctx, &mcp.CallToolParams{Name: "wait_session", Arguments: args})
				if err != nil {
					t.Fatal(err)
				}
				var message string
				if len(result.Content) > 0 {
					if content, ok := result.Content[0].(*mcp.TextContent); ok {
						message = content.Text
					}
				}
				if !result.IsError || !strings.HasPrefix(message, "deadline_exceeded:") {
					t.Errorf("wait_session = %q (isError %v), want deadline_exceeded tool error", message, result.IsError)
				}
			}
			if got := calls.Load(); got != 1 {
				t.Errorf("GetDevSession calls = %d, want 1", got)
			}
		})
	}
}

func TestExecTimeoutRange(t *testing.T) {
	needPOSIX(t)
	h := newHarness(t, Config{})
	args := map[string]any{"session_id": h.running(), "command": []string{"sh", "-c", "exit 0"}}

	for _, seconds := range []int64{-1, 9223372037, 18446744074} {
		args["timeout_seconds"] = seconds
		if text, isErr := h.call("exec", args, nil); !isErr || !strings.HasPrefix(text, "invalid_argument:") {
			t.Errorf("timeout_seconds %d = %q, isError = %v", seconds, text, isErr)
		}
		if got := len(h.srv.ExecRequests()); got != 0 {
			t.Fatalf("timeout_seconds %d sent %d exec requests", seconds, got)
		}
	}

	args["timeout_seconds"] = int64(9223372036)
	if text, isErr := h.call("exec", args, nil); isErr {
		t.Fatalf("maximum timeout: %s", text)
	}
	requests := h.srv.ExecRequests()
	if len(requests) != 1 {
		t.Fatalf("got %d exec requests, want 1", len(requests))
	}
	if got := requests[0].GetTimeout().AsDuration(); got != 9223372036*time.Second {
		t.Fatalf("maximum timeout = %v, want 9223372036 seconds", got)
	}

	args["timeout_seconds"] = 0
	if text, isErr := h.call("exec", args, nil); isErr {
		t.Fatalf("zero timeout: %s", text)
	}
	requests = h.srv.ExecRequests()
	if len(requests) != 2 {
		t.Fatalf("got %d exec requests, want 2", len(requests))
	}
	if requests[1].GetTimeout() != nil {
		t.Fatalf("zero timeout sent %v, want absent field", requests[1].GetTimeout())
	}
}

func TestExecWithinAndBeyondWait(t *testing.T) {
	needPOSIX(t)
	h := newHarness(t, Config{})
	id := h.running()

	var r execResult
	text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"sh", "-c", "echo hello; echo oops >&2; exit 4"}}, &r)
	if isErr || r.Running || r.ExitCode == nil || *r.ExitCode != 4 || r.Stdout.Text != "hello\n" || r.Stderr.Text != "oops\n" {
		t.Fatalf("short exec = %q %+v", text, r)
	}
	if r.NextStdoutOffset != 6 || r.Stdout.Omitted != nil {
		t.Fatalf("offsets %+v", r)
	}

	// A command that runs longer than the wait keeps running.
	text, isErr = h.call("exec", map[string]any{"session_id": id, "command": []string{"sh", "-c", "echo begin; sleep 2; echo finish"}, "wait_seconds": 0}, &r)
	if isErr || !r.Running || r.ExecID == "" || r.ExitCode != nil {
		t.Fatalf("long exec = %q %+v", text, r)
	}
	var out execResult
	var all strings.Builder
	all.WriteString(r.Stdout.Text)
	so, se := r.NextStdoutOffset, r.NextStderrOffset
	for range 10 {
		text, isErr = h.call("exec_output", map[string]any{"session_id": id, "exec_id": r.ExecID, "stdout_offset": so, "stderr_offset": se, "wait_seconds": 5}, &out)
		if isErr {
			t.Fatalf("exec_output: %s", text)
		}
		all.WriteString(out.Stdout.Text)
		so, se = out.NextStdoutOffset, out.NextStderrOffset
		if !out.Running {
			break
		}
	}
	if out.Running || out.ExitCode == nil || *out.ExitCode != 0 || all.String() != "begin\nfinish\n" {
		t.Fatalf("exec_output end = %+v, output %q", out, all.String())
	}

	// A timeout stops the command.
	if _, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"sleep", "10"}, "timeout_seconds": 1}, &r); isErr || !r.TimedOut {
		t.Fatalf("timeout exec = %+v", r)
	}
}

type attachExecFixture struct {
	sandrunv1connect.UnimplementedDevSessionServiceHandler
	attach func(context.Context, *sandrunv1.AttachDevSessionExecRequest, *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error
}

func (f attachExecFixture) AttachDevSessionExec(ctx context.Context, req *connect.Request[sandrunv1.AttachDevSessionExecRequest], stream *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error {
	return f.attach(ctx, req.Msg, stream)
}

func TestExecOutputAttachConfirmation(t *testing.T) {
	const (
		execID       = "test-exec"
		stdoutOffset = 12
		stderrOffset = 34
		maxWait      = time.Second + 200*time.Millisecond
		deadline     = "deadline_exceeded: exec_output did not confirm exec test-exec before the wait expired. Retry with the same exec_id, stdout_offset 12, and stderr_offset 34."
	)
	keepalive := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Keepalive{Keepalive: &sandrunv1.DevSessionExecKeepalive{}}}
	output := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Stdout{
		Stdout: &sandrunv1.DevSessionExecOutput{Offset: stdoutOffset, Data: []byte("hi")},
	}}
	truncated := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_OutputTruncated{
		OutputTruncated: &sandrunv1.DevSessionExecOutputTruncated{StdoutOffset: 16, StderrOffset: stderrOffset},
	}}
	exited := &sandrunv1.DevSessionExecEvent{Event: &sandrunv1.DevSessionExecEvent_Exited{
		Exited: &sandrunv1.DevSessionExecExited{Status: &sandrunv1.DevSessionExecExited_ExitCode{ExitCode: 7}},
	}}
	exitCode := int64(7)
	tests := []struct {
		name       string
		wait       int
		events     []*sandrunv1.DevSessionExecEvent
		code       connect.Code
		wantErr    string
		running    bool
		stdout     string
		stdoutNext int64
		missing    *sandrun.ByteRange
		exit       *int64
	}{
		{name: "silent wait", wait: 1, wantErr: deadline},
		{name: "silent poll", wantErr: deadline},
		{name: "keepalive wait", wait: 1, events: []*sandrunv1.DevSessionExecEvent{keepalive}, wantErr: deadline},
		{name: "keepalive poll", events: []*sandrunv1.DevSessionExecEvent{keepalive}, wantErr: deadline},
		{name: "missing", code: connect.CodeNotFound, wantErr: "not_found: attach rejected"},
		{name: "denied", code: connect.CodePermissionDenied, wantErr: "permission_denied: attach rejected"},
		{name: "running", events: []*sandrunv1.DevSessionExecEvent{output}, running: true, stdout: "hi", stdoutNext: 14},
		{name: "truncated", events: []*sandrunv1.DevSessionExecEvent{truncated}, running: true, stdoutNext: 16, missing: &sandrun.ByteRange{From: 12, To: 16}},
		{name: "exited silently", events: []*sandrunv1.DevSessionExecEvent{exited}, stdoutNext: stdoutOffset, exit: &exitCode},
		{name: "exited with output", wait: 1, events: []*sandrunv1.DevSessionExecEvent{output, exited}, stdout: "hi", stdoutNext: 14, exit: &exitCode},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, handler := sandrunv1connect.NewDevSessionServiceHandler(attachExecFixture{attach: func(ctx context.Context, req *sandrunv1.AttachDevSessionExecRequest, stream *connect.ServerStream[sandrunv1.AttachDevSessionExecResponse]) error {
				if req.GetExecId() != execID || req.GetStdoutOffset() != stdoutOffset || req.GetStderrOffset() != stderrOffset {
					t.Errorf("attach request = %v", req)
				}
				if tc.code != 0 {
					return connect.NewError(tc.code, errors.New("attach rejected"))
				}
				for _, event := range tc.events {
					if err := stream.Send(&sandrunv1.AttachDevSessionExecResponse{Event: event}); err != nil {
						return err
					}
				}
				if tc.exit != nil {
					return nil
				}
				<-ctx.Done()
				return ctx.Err()
			}})
			upstream := httptest.NewUnstartedServer(handler)
			upstream.Config.Protocols = new(http.Protocols)
			upstream.Config.Protocols.SetUnencryptedHTTP2(true)
			upstream.Start()
			t.Cleanup(upstream.Close)
			h := newHarness(t, Config{maxWait: maxWait, Client: func(context.Context) (*sandrun.Client, error) {
				return sandrun.New(sandrun.Options{BaseURL: upstream.URL, APIKey: "test-key"})
			}})
			var out execResult
			start := time.Now()
			message, isErr := h.call("exec_output", map[string]any{
				"session_id": "test-session", "exec_id": execID, "stdout_offset": stdoutOffset,
				"stderr_offset": stderrOffset, "wait_seconds": tc.wait,
			}, &out)
			if elapsed := time.Since(start); elapsed > maxWait+time.Second {
				t.Errorf("attach took %v, cap is %v", elapsed, maxWait)
			}
			if tc.wantErr != "" {
				if !isErr || message != tc.wantErr {
					t.Fatalf("attach error = %v %q, want %q", isErr, message, tc.wantErr)
				}
				return
			}
			if isErr || out.ExecID != execID || out.Running != tc.running || !reflect.DeepEqual(out.ExitCode, tc.exit) {
				t.Fatalf("attach result = %q %+v", message, out)
			}
			if out.Stdout != (Output{Text: tc.stdout, From: stdoutOffset, To: tc.stdoutNext}) ||
				out.Stderr != (Output{From: stderrOffset, To: stderrOffset}) ||
				out.NextStdoutOffset != tc.stdoutNext || out.NextStderrOffset != stderrOffset {
				t.Fatalf("attach output and offsets = %+v", out)
			}
			if tc.missing != nil && !reflect.DeepEqual(out.OutputTruncated, []Truncated{{MissingStdout: tc.missing}}) {
				t.Fatalf("attach truncation = %+v", out.OutputTruncated)
			}
		})
	}
}

func TestExecOutputWindowAndOffsets(t *testing.T) {
	needPOSIX(t)
	h := newHarness(t, Config{})
	id := h.running()
	// 100000 bytes of stdout: the result keeps 8 KiB + 24 KiB and states
	// the omitted range.
	cmd := []string{"sh", "-c", "i=0; while [ $i -lt 1000 ]; do printf '%099d\\n' $i; i=$((i+1)); done"}
	var r execResult
	text, isErr := h.call("exec", map[string]any{"session_id": id, "command": cmd}, &r)
	if isErr || r.ExitCode == nil {
		t.Fatalf("exec = %q", text)
	}
	if r.Stdout.To != 100000 || r.Stdout.Omitted == nil {
		t.Fatalf("window = from %d to %d omitted %v", r.Stdout.From, r.Stdout.To, r.Stdout.Omitted)
	}
	if *r.Stdout.Omitted != (sandrun.ByteRange{From: headBytes, To: 100000 - tailBytes}) {
		t.Fatalf("omitted = %+v", *r.Stdout.Omitted)
	}
	if !strings.HasPrefix(r.Stdout.Text, strings.Repeat("0", 99)+"\n") || !strings.HasSuffix(r.Stdout.Text, "000000999\n") {
		t.Fatal("window text lacks the head or the tail")
	}
	// The omitted range can be read again from its offset.
	var out execResult
	if text, isErr := h.call("exec_output", map[string]any{"session_id": id, "exec_id": r.ExecID, "stdout_offset": r.Stdout.Omitted.From, "stderr_offset": 0}, &out); isErr {
		t.Fatalf("exec_output: %s", text)
	}
	if out.Stdout.From != headBytes || out.Stdout.To != 100000 || out.ExitCode == nil {
		t.Fatalf("exec_output from offset = %+v", out.Stdout)
	}
	if out.Stdout.Omitted == nil || out.Stdout.Omitted.From != 2*headBytes {
		t.Fatalf("exec_output window %+v", out.Stdout.Omitted)
	}

	// Output that the guest no longer keeps is reported.
	h.srv.OutputKeepBytes = 100
	if _, isErr := h.call("exec", map[string]any{"session_id": id, "command": cmd}, &r); isErr {
		t.Fatal("exec")
	}
	if text, isErr := h.call("exec_output", map[string]any{"session_id": id, "exec_id": r.ExecID, "stdout_offset": 0, "stderr_offset": 0}, &out); isErr || len(out.OutputTruncated) == 0 {
		t.Fatalf("truncated = %q %+v", text, out.OutputTruncated)
	}
	if m := out.OutputTruncated[0].MissingStdout; m == nil || m.From != 0 || m.To != 100000-100 {
		t.Fatalf("missing = %+v", m)
	}
	if out.Stdout != (Output{Text: strings.Repeat("0", 96) + "999\n", To: 100000}) || out.NextStdoutOffset != 100000 {
		t.Fatalf("short retained output = %+v, next = %d", out.Stdout, out.NextStdoutOffset)
	}

	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream+" truncated prefix", func(t *testing.T) {
			h := newHarness(t, Config{})
			h.srv.OutputKeepBytes = 65536
			id := h.running()
			script := `emit() { i=0; while [ "$i" -lt 1024 ]; do printf '%04d%0123d\n' "$i" 0; i=$((i+1)); done; }; `
			if stream == "stderr" {
				script += "printf short; emit >&2"
			} else {
				script += "printf short >&2; emit"
			}
			var run execResult
			if msg, bad := h.call("exec", map[string]any{"session_id": id, "command": []string{"sh", "-c", script}}, &run); bad || run.ExitCode == nil || *run.ExitCode != 0 {
				t.Fatalf("exec: %s", msg)
			}
			args := map[string]any{"session_id": id, "exec_id": run.ExecID, "stdout_offset": 0, "stderr_offset": 0}
			var result execResult
			if msg, bad := h.call("exec_output", args, &result); bad || result.ExitCode == nil {
				t.Fatalf("exec_output: %s", msg)
			}
			preview, other := result.Stdout, result.Stderr
			next, otherNext := result.NextStdoutOffset, result.NextStderrOffset
			missing := &sandrun.ByteRange{From: 0, To: 65536}
			wantTruncated := Truncated{MissingStdout: missing}
			if stream == "stderr" {
				preview, other = other, preview
				next, otherNext = otherNext, next
				wantTruncated = Truncated{MissingStderr: missing}
			}
			if !reflect.DeepEqual(result.OutputTruncated, []Truncated{wantTruncated}) {
				t.Fatalf("truncated = %+v, want %+v", result.OutputTruncated, wantTruncated)
			}
			wantOmitted := sandrun.ByteRange{From: 73728, To: 106496}
			if preview.From != 0 || preview.To != 131072 || next != 131072 || preview.Omitted == nil || *preview.Omitted != wantOmitted {
				t.Fatalf("%s from = %d, to = %d, next = %d, omitted = %+v", stream, preview.From, preview.To, next, preview.Omitted)
			}
			if other != (Output{Text: "short", To: 5}) || otherNext != 5 {
				t.Fatalf("other stream = %+v, next = %d", other, otherNext)
			}
			args[stream+"_offset"] = preview.Omitted.From
			if msg, bad := h.call("exec_output", args, &result); bad || len(result.OutputTruncated) != 0 {
				t.Fatalf("resume: %s, truncated = %+v", msg, result.OutputTruncated)
			}
			resumed := result.Stdout
			if stream == "stderr" {
				resumed = result.Stderr
			}
			if resumed.From != 73728 || !strings.HasPrefix(resumed.Text, "0576") {
				t.Fatalf("resumed %s does not start at byte 73728", stream)
			}
		})
	}
}

func TestExecOutputWindowAndOffsetsWithGaps(t *testing.T) {
	for _, tc := range []struct {
		name     string
		before   int
		after    int
		wantText string
		wantOmit *sandrun.ByteRange
	}{
		{
			name:     "gap in head",
			before:   3000,
			after:    40000,
			wantText: strings.Repeat("a", 3000) + strings.Repeat("b", 5192) + "\n[... output omitted ...]\n" + strings.Repeat("b", 24576),
			wantOmit: &sandrun.ByteRange{From: 55192, To: 65424},
		},
		{
			name:     "gap in tail",
			before:   28192,
			after:    20000,
			wantText: strings.Repeat("a", 8192) + "\n[... output omitted ...]\n" + strings.Repeat("b", 20000),
			wantOmit: &sandrun.ByteRange{From: 8192, To: 50000},
		},
		{
			name:     "all received bytes kept",
			before:   3000,
			after:    20000,
			wantText: strings.Repeat("a", 3000) + strings.Repeat("b", 20000),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const afterOffset = 50000
			c := newCollector(0, 7)
			missing := sandrun.ByteRange{From: int64(tc.before), To: afterOffset}
			for _, ev := range []sandrun.ExecEvent{
				{Kind: sandrun.ExecEventStdout, Data: []byte(strings.Repeat("a", tc.before))},
				{Kind: sandrun.ExecEventOutputTruncated, MissingStdout: missing},
				{Kind: sandrun.ExecEventStdout, Offset: afterOffset, Data: []byte(strings.Repeat("b", tc.after))},
			} {
				if err := c.on(ev); err != nil {
					t.Fatal(err)
				}
			}
			r := c.result("exec", nil)
			wantNext := int64(afterOffset + tc.after)
			if r.Stdout.From != 0 || r.Stdout.To != wantNext || r.NextStdoutOffset != wantNext || !reflect.DeepEqual(r.Stdout.Omitted, tc.wantOmit) {
				t.Fatalf("stdout from = %d, to = %d, next = %d, omitted = %+v, want omitted = %+v", r.Stdout.From, r.Stdout.To, r.NextStdoutOffset, r.Stdout.Omitted, tc.wantOmit)
			}
			if r.Stdout.Text != tc.wantText {
				t.Fatal("stdout preview differs from the expected text")
			}
			if r.Stderr != (Output{From: 7, To: 7}) || r.NextStderrOffset != 7 || !reflect.DeepEqual(r.OutputTruncated, []Truncated{{MissingStdout: &missing}}) {
				t.Fatalf("stderr = %+v, next = %d, truncated = %+v", r.Stderr, r.NextStderrOffset, r.OutputTruncated)
			}
		})
	}
}

func TestPassEnvValuesNeverInResults(t *testing.T) {
	needPOSIX(t)
	const secret = "s3cr3t-value-for-test"
	env := map[string]string{"NPM_TOKEN": secret, "EMPTY_TOKEN": ""}
	h := newHarness(t, Config{
		PassEnv:   []string{"NPM_TOKEN", "MISSING_TOKEN", "EMPTY_TOKEN"},
		LookupEnv: func(k string) (string, bool) { v, ok := env[k]; return v, ok },
	})
	id := h.running()
	var r execResult
	text, isErr := h.call("exec", map[string]any{
		"session_id": id,
		"command":    []string{"sh", "-c", `test "$NPM_TOKEN" = "` + secret + `" && echo ok; test -z "$MISSING_TOKEN" && echo unset`},
		"env": map[string]string{
			"NPM_TOKEN":     "agent-value",
			"MISSING_TOKEN": "agent-missing-value",
			"EMPTY_TOKEN":   "agent-empty-value",
			"BUILD_MODE":    "agent-mode",
		},
	}, &r)
	if isErr {
		t.Fatalf("exec = %q %+v", text, r)
	}
	if strings.Contains(text, secret) {
		t.Fatal("the --pass-env value appears in the tool result")
	}
	if !slices.Equal(r.MissingEnv, []string{"MISSING_TOKEN"}) {
		t.Fatalf("missing_env = %v", r.MissingEnv)
	}
	reqs := h.srv.ExecRequests()
	requestEnv := reqs[len(reqs)-1].GetEnv()
	if requestEnv["NPM_TOKEN"] != secret {
		t.Fatal("the --pass-env value did not reach the guest")
	}
	if _, ok := requestEnv["MISSING_TOKEN"]; ok {
		t.Fatal("the exec request contains the unset --pass-env variable")
	}
	if v, ok := requestEnv["EMPTY_TOKEN"]; !ok || v != "" {
		t.Fatal("the exec request does not contain the empty --pass-env value")
	}
	if requestEnv["BUILD_MODE"] != "agent-mode" {
		t.Fatal("the exec request did not retain the tool-supplied value")
	}
	if r.Stdout.Text != "ok\nunset\n" {
		t.Fatalf("stdout = %q", r.Stdout.Text)
	}
	// A tool call cannot read the value back through the output either
	// except by printing it itself; the server adds nothing of it.
	res, _ := h.session.ListTools(context.Background(), nil)
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), secret) {
		t.Fatal("the --pass-env value appears in the tool list")
	}
}

func TestSyncAndDownloadThroughTransferStatus(t *testing.T) {
	needPOSIX(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	run("init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "main.c"), []byte("int main(){}"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := newHarness(t, Config{WorkDir: repo})
	id := h.running()

	var r transferResult
	text, isErr := h.call("sync", map[string]any{"session_id": id, "wait_seconds": 0}, &r)
	if isErr || r.TransferID == "" || r.Kind != "sync" {
		t.Fatalf("sync = %q %+v", text, r)
	}
	if r.Running && r.Progress == nil {
		t.Fatal("a running transfer has no progress")
	}
	if text, isErr := h.call("transfer_status", map[string]any{"transfer_id": r.TransferID}, &r); isErr || r.Running || r.Sync == nil || r.Sync.Uploaded != 1 {
		t.Fatalf("transfer_status = %q %+v", text, r)
	}
	if b, err := os.ReadFile(filepath.Join(h.ws, "main.c")); err != nil || string(b) != "int main(){}" {
		t.Fatal("the sync did not copy the file")
	}

	if err := os.WriteFile(filepath.Join(h.ws, "a.out"), []byte("built"), 0o755); err != nil {
		t.Fatal(err)
	}
	text, isErr = h.call("download", map[string]any{"session_id": id, "guest_path": "a.out", "local_path": "out/a.out", "wait_seconds": 0}, &r)
	if isErr {
		t.Fatalf("download = %q", text)
	}
	if r.Running {
		if text, isErr = h.call("transfer_status", map[string]any{"transfer_id": r.TransferID}, &r); isErr {
			t.Fatalf("transfer_status = %q", text)
		}
	}
	if r.Download == nil || r.Download.Files != 1 || r.Download.LocalPath != filepath.Join(repo, "out/a.out") {
		t.Fatalf("download result %+v", r)
	}
	if text, isErr := h.call("transfer_status", map[string]any{"transfer_id": "missing"}, nil); !isErr || !strings.HasPrefix(text, "not_found") {
		t.Fatalf("unknown transfer = %q", text)
	}

	// A failed transfer reports its error with the Connect code.
	h.srv.SetSessionState(id, sandrunv1.RequestState_REQUEST_STATE_STARTING)
	if text, isErr := h.call("sync", map[string]any{"session_id": id}, nil); !isErr || !strings.HasPrefix(text, "failed_precondition") {
		t.Fatalf("sync of a starting session = %q", text)
	}
}

func TestErrorMapping(t *testing.T) {
	h := newHarness(t, Config{})
	if text, isErr := h.call("create_session", map[string]any{"profile": "missing"}, nil); !isErr || !strings.HasPrefix(text, "not_found") {
		t.Fatalf("missing profile = %q", text)
	}
	// The per-user dev session limit.
	ctx := context.Background()
	c, _ := sandrun.New(sandrun.Options{BaseURL: h.srv.URL, APIKey: h.srv.AccessToken})
	one := int32(1)
	if _, err := c.Organizations().UpdateOrganizationSettings(ctx, connect.NewRequest(&sandrunv1.UpdateOrganizationSettingsRequest{
		OrganizationId: h.org, Settings: &sandrunv1.OrganizationSettings{MaxDevSessionsPerUser: &one}, UpdateMask: fieldMask("max_dev_sessions_per_user"),
	})); err != nil {
		t.Fatal(err)
	}
	id := h.running()
	if text, isErr := h.call("create_session", map[string]any{"profile": "linux"}, nil); !isErr || !strings.HasPrefix(text, "resource_exhausted") {
		t.Fatalf("limit = %q", text)
	}
	h.srv.SetSessionState(id, sandrunv1.RequestState_REQUEST_STATE_QUEUED)
	if text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"true"}}, nil); !isErr || !strings.HasPrefix(text, "failed_precondition") {
		t.Fatalf("exec in a queued session = %q", text)
	}
	var d sessionResult
	if text, isErr := h.call("delete_session", map[string]any{"session_id": id}, &d); isErr || d.Session.State != "canceled" || d.Session.EndReason != "canceled_by_user" {
		t.Fatalf("delete = %q %+v", text, d)
	}
	var l listSessionsResult
	if _, isErr := h.call("list_sessions", map[string]any{"include_ended": true}, &l); isErr || len(l.Sessions) != 1 {
		t.Fatalf("list = %+v", l)
	}
	if text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{}}, nil); !isErr || !strings.HasPrefix(text, "invalid_argument") {
		t.Fatalf("empty command = %q", text)
	}

	// Without a credential, every tool tells the user what to do.
	noCred := newHarness(t, Config{Client: func(context.Context) (*sandrun.Client, error) {
		return nil, errors.New("not signed in: run 'sandrun login' or set SANDRUN_API_KEY")
	}})
	for _, tool := range []string{"list_profiles", "list_sessions"} {
		if text, isErr := noCred.call(tool, map[string]any{}, nil); !isErr || !strings.Contains(text, "sandrun login") || !strings.Contains(text, "SANDRUN_API_KEY") {
			t.Fatalf("%s without credential = %q", tool, text)
		}
	}
	var p listProfilesResult
	if _, isErr := h.call("list_profiles", map[string]any{}, &p); isErr || len(p.Profiles) != 1 || p.Profiles[0].GuestOS != "linux" || p.Profiles[0].MaxQueueWait != "30m0s" {
		t.Fatalf("profiles = %+v", p)
	}
}

func fieldMask(paths ...string) *fieldmaskpb.FieldMask { return &fieldmaskpb.FieldMask{Paths: paths} }

// TestExecStartUnconfirmed checks that an exec whose start is not
// confirmed before the wait passes is an error, not a running exec. A
// token source that blocks, like a slow Logto refresh, holds the exec
// request until the stream is canceled.
func TestExecStartUnconfirmed(t *testing.T) {
	var block atomic.Bool
	var srv *sandruntest.Server
	h := newHarness(t, Config{maxWait: time.Second, Client: func(context.Context) (*sandrun.Client, error) {
		return sandrun.New(sandrun.Options{BaseURL: srv.URL, TokenSource: func(ctx context.Context) (string, error) {
			if block.Load() {
				<-ctx.Done()
				return "", ctx.Err()
			}
			return srv.AccessToken, nil
		}})
	}})
	srv = h.srv
	id := h.running()
	block.Store(true)
	text, isErr := h.call("exec", map[string]any{"session_id": id, "command": []string{"true"}, "wait_seconds": 1}, nil)
	if !isErr || !strings.HasPrefix(text, "deadline_exceeded") || !strings.Contains(text, "did not confirm its start") {
		t.Fatalf("unconfirmed exec = %q %v", text, isErr)
	}
	if n := len(h.srv.ExecRequests()); n != 0 {
		t.Fatalf("exec requests = %d", n)
	}
}
