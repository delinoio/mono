// SPDX-License-Identifier: Apache-2.0

package tide

import (
	"bytes"
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/tide/tidetest"
	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
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
func newSession(t *testing.T, c *Client, srv *tidetest.Server, org string, os tidev1.GuestOs) *tidev1.DevSession {
	t.Helper()
	ws := t.TempDir() + "/work"
	if err := mkdir(ws); err != nil {
		t.Fatal(err)
	}
	prof := srv.AddDevSessionProfile(org, "p-"+strings.ToLower(os.String()), os, ws)
	res, err := c.DevSessions().CreateDevSession(context.Background(), connect.NewRequest(&tidev1.CreateDevSessionRequest{OrganizationId: org, RunnerProfileId: prof.GetRunnerProfileId()}))
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

func TestExecAttachAfterStreamRotation(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	srv.ExecStreamEventLimit = 1
	var got collected
	res, err := c.Exec(context.Background(), ExecParams{
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
	if got.stdout.String() != wantOut || !strings.Contains(got.stderr.String(), "err 6") || got.started != 1 {
		t.Fatalf("stdout %q stderr %q started %d", got.stdout.String(), got.stderr.String(), got.started)
	}
	if res.StdoutOffset != int64(len(wantOut)) {
		t.Fatalf("stdout offset %d", res.StdoutOffset)
	}
	if n := srv.Calls("/delino.tide.v1.DevSessionService/AttachDevSessionExec"); n < 2 {
		t.Fatalf("attach calls = %d, want several", n)
	}
	if n := srv.Calls("/delino.tide.v1.DevSessionService/ExecDevSession"); n != 1 {
		t.Fatalf("exec calls = %d, want 1", n)
	}
	reqs := srv.ExecRequests()
	if len(reqs) != 1 || len(reqs[0].GetExecId()) != 36 {
		t.Fatalf("exec requests %v", reqs)
	}
}

func TestExecReportsLostOutput(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	srv.ExecStreamEventLimit = 1
	srv.OutputKeepBytes = 8
	var got collected
	res, err := c.Exec(context.Background(), ExecParams{
		OrganizationID: org, DevSessionID: ds.GetDevSessionId(),
		Command: []string{"sh", "-c", `echo first; sleep 0.2; echo 0123456789abcdefghij; sleep 0.2; echo end`},
		OnEvent: got.on,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Incomplete || len(got.truncated) == 0 {
		t.Fatalf("result incomplete=%v truncated=%v", res.Incomplete, got.truncated)
	}
	tr := got.truncated[0].MissingStdout
	if tr.Empty() || tr.From < int64(len("first\n")) {
		t.Fatalf("missing range %+v", tr)
	}
	if !strings.HasSuffix(got.stdout.String(), "end\n") {
		t.Fatalf("stdout %q", got.stdout.String())
	}
}

func TestExecErrors(t *testing.T) {
	needPOSIX(t)
	c, srv, org := newTestClient(t, "")
	ds := newSession(t, c, srv, org, tidev1.GuestOs_GUEST_OS_LINUX)
	ctx := context.Background()

	res, err := c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"tide-no-such-program"}})
	if err != nil || res.Kind != ExecResultFailed || res.FailureReason != tidev1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND {
		t.Fatalf("missing program: %+v %v", res, err)
	}
	res, err = c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"sleep", "5"}, Timeout: 50 * time.Millisecond})
	if err != nil || !res.TimedOut {
		t.Fatalf("timeout: %+v %v", res, err)
	}

	// A server error of ExecDevSession is returned with its code.
	srv.SetSessionState(ds.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_QUEUED)
	_, err = c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"true"}})
	var se *ExecStreamError
	if connect.CodeOf(err) != connect.CodeFailedPrecondition || errors.As(err, &se) {
		t.Fatalf("not running: %v", err)
	}
	srv.SetSessionState(ds.GetDevSessionId(), tidev1.RequestState_REQUEST_STATE_RUNNING)

	// An unknown exec on attach reports an unknown outcome.
	_, err = c.AttachExec(ctx, AttachParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), ExecID: "missing"})
	if !errors.As(err, &se) || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("attach missing: %v", err)
	}

	// A cancelled caller gets the offsets to continue with.
	cctx, cancel := context.WithCancel(ctx)
	var got collected
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

	// A transient failure of the first stream continues with attach, and
	// the command runs once.
	srv.Fail("/delino.tide.v1.DevSessionService/AttachDevSessionExec", connect.NewError(connect.CodeUnavailable, errors.New("restart")))
	srv.ExecStreamEventLimit = 1
	got = collected{}
	res, err = c.Exec(ctx, ExecParams{OrganizationID: org, DevSessionID: ds.GetDevSessionId(), Command: []string{"sh", "-c", "echo a; echo b"}, OnEvent: got.on})
	if err != nil || got.stdout.String() != "a\nb\n" {
		t.Fatalf("retry: %q %v", got.stdout.String(), err)
	}
	_ = res
}

func mkdir(p string) error { return os.MkdirAll(p, 0o755) }
