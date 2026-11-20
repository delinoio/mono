// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package guest

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func execReq(t *testing.T, cmd ...string) *ownsandv1.ExecRequest {
	return &ownsandv1.ExecRequest{SandboxId: testSandbox, ExecId: newID(t), Command: cmd}
}

func TestExecOutputAndExitCode(t *testing.T) {
	h := newHarness(t, nil)
	req := execReq(t, "/bin/sh", "-c", `printf out; printf err >&2; echo "$FOO"; pwd; exit 3`)
	req.Env = map[string]string{"FOO": "bar"}
	req.WorkingDirectory = t.TempDir()
	evs := h.runExec(req)
	if evs[0].GetStarted().GetExecId() != req.GetExecId() || evs[0].GetStarted().GetStartTime() == nil {
		t.Fatalf("first event %v", evs[0])
	}
	stdout, stderr := outputs(evs)
	if stdout != "outbar\n"+req.GetWorkingDirectory()+"\n" || stderr != "err" {
		t.Fatalf("stdout %q stderr %q", stdout, stderr)
	}
	last := evs[len(evs)-1].GetExited()
	if last.GetExitCode() != 3 || last.GetTimedOut() || last.GetEndTime() == nil {
		t.Fatalf("exited %v", last)
	}
}

func TestExecSignal(t *testing.T) {
	h := newHarness(t, nil)
	evs := h.runExec(execReq(t, "/bin/sh", "-c", "kill -KILL $$"))
	ex := evs[len(evs)-1].GetExited()
	if _, ok := ex.GetStatus().(*ownsandv1.ExecExited_Signal); !ok || ex.GetSignal() != int32(syscall.SIGKILL) {
		t.Fatalf("exited %v", ex)
	}
}

func TestExecLargeOutputChunks(t *testing.T) {
	h := newHarness(t, nil)
	evs := h.runExec(execReq(t, "/bin/sh", "-c", "head -c 300000 /dev/zero"))
	var next int64
	for _, ev := range evs {
		if out := ev.GetStdout(); out != nil {
			if out.GetOffset() != next || len(out.GetData()) > 64<<10 {
				t.Fatalf("chunk offset %d size %d, want offset %d", out.GetOffset(), len(out.GetData()), next)
			}
			next += int64(len(out.GetData()))
		}
	}
	if next != 300000 {
		t.Fatalf("got %d bytes", next)
	}
}

func TestExecStartFailures(t *testing.T) {
	h := newHarness(t, nil)
	cases := []struct {
		req  *ownsandv1.ExecRequest
		want ownsandv1.ExecFailureReason
	}{
		{execReq(t, "/nonexistent/program"), ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND},
		{execReq(t, "no-such-program-ownsand"), ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PROGRAM_NOT_FOUND},
		{execReq(t, "/dev/null"), ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_PERMISSION_DENIED},
	}
	bad := execReq(t, "/bin/true")
	bad.WorkingDirectory = "/nonexistent/dir"
	cases = append(cases, struct {
		req  *ownsandv1.ExecRequest
		want ownsandv1.ExecFailureReason
	}{bad, ownsandv1.ExecFailureReason_EXEC_FAILURE_REASON_WORKING_DIRECTORY_INVALID})
	for _, c := range cases {
		evs := h.runExec(c.req)
		if len(evs) != 2 || evs[0].GetStarted() == nil || evs[1].GetFailed().GetReason() != c.want {
			t.Errorf("%v: events %v", c.req.GetCommand(), evs)
		}
	}
}

func TestExecTimeoutTerminatesThenKills(t *testing.T) {
	h := newHarness(t, nil)
	req := execReq(t, "/bin/sh", "-c", "sleep 30")
	req.Timeout = durationpb.New(100 * time.Millisecond)
	start := time.Now()
	evs := h.runExec(req)
	ex := evs[len(evs)-1].GetExited()
	if !ex.GetTimedOut() || ex.GetSignal() != int32(syscall.SIGTERM) {
		t.Fatalf("exited %v", ex)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout took too long")
	}

	h.srv.execs.mu.Lock()
	h.srv.execs.killGrace = 300 * time.Millisecond
	h.srv.execs.mu.Unlock()
	req = execReq(t, "/bin/sh", "-c", "trap '' TERM; echo ready; while :; do sleep 0.05; done")
	req.Timeout = durationpb.New(200 * time.Millisecond)
	evs = h.runExec(req)
	ex = evs[len(evs)-1].GetExited()
	if !ex.GetTimedOut() || ex.GetSignal() != int32(syscall.SIGKILL) {
		t.Fatalf("exited after ignored SIGTERM: %v", ex)
	}
}

func TestExecRunsIndependentlyAndAttach(t *testing.T) {
	h := newHarness(t, nil)
	req := execReq(t, "/bin/sh", "-c", "printf hello; sleep 0.3; printf world")
	ctx, cancel := context.WithCancel(context.Background())
	s := h.open(ctx)
	s.send(1, req)
	if f := s.recv(); f.GetExecEvent().GetStarted() == nil {
		t.Fatalf("first frame %v", f)
	}
	cancel() // The stream ends; the command keeps running.

	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	s2 := h.open(ctx2)
	s2.send(5, &ownsandv1.AttachExecRequest{SandboxId: testSandbox, ExecId: req.GetExecId(), StdoutOffset: 2})
	evs := s2.collectExec()
	stdout, _ := outputs(evs)
	if stdout != "lloworld" || evs[len(evs)-1].GetExited().GetExitCode() != 0 {
		t.Fatalf("attach events %v", evs)
	}
	// A repeated exec request attaches from offset 0 and does not run the
	// command again.
	evs = h.runExec(req)
	if stdout, _ := outputs(evs); stdout != "helloworld" {
		t.Fatalf("repeated exec stdout %q", stdout)
	}

	s2.send(6, &ownsandv1.AttachExecRequest{SandboxId: testSandbox, ExecId: newID(t)})
	if f := s2.recv(); f.GetStreamId() != 6 || f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND {
		t.Fatalf("unknown attach %v", f)
	}
}

func TestExecRetention(t *testing.T) {
	h := newHarness(t, nil)
	req := execReq(t, "/bin/true")
	h.runExec(req)
	h.clock.Advance(9 * time.Minute)
	h.srv.Sweep()
	if _, ok := h.srv.execs.lookup(req.GetExecId()); !ok {
		t.Fatal("exec forgotten before 10 minutes")
	}
	h.clock.Advance(2 * time.Minute)
	h.srv.Sweep()
	if _, ok := h.srv.execs.lookup(req.GetExecId()); ok {
		t.Fatal("exec kept after 10 minutes")
	}
}

func TestExecKeepalive(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KeepaliveInterval = 50 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := h.open(ctx)
	s.send(1, execReq(t, "/bin/sh", "-c", "sleep 0.3"))
	sawKeepalive := false
	for {
		ev := s.recv().GetExecEvent()
		if ev.GetKeepalive() != nil {
			sawKeepalive = true
		}
		if ev.GetExited() != nil {
			break
		}
	}
	if !sawKeepalive {
		t.Fatal("no keepalive while the command was silent")
	}
}

func TestCommandEnvOverridesHelperEnv(t *testing.T) {
	t.Setenv("OWNSAND_GUEST_TEST_VAR", "helper")
	env := strings.Join(commandEnv(map[string]string{"OWNSAND_GUEST_TEST_VAR": "request"}), "\n")
	if !strings.Contains(env, "OWNSAND_GUEST_TEST_VAR=request") || strings.Contains(env, "=helper") {
		t.Fatal("request env did not override the helper env")
	}
}
