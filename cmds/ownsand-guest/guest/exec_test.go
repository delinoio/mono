// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package guest

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
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
	// `pwd` prints the physical path: the helper passes its own PWD, which
	// does not name the working directory, so the shell does not use it. On
	// macOS the TempDir path starts with /var, a symbolic link to
	// /private/var, so compare with the resolved path.
	wantDir, err := filepath.EvalSymlinks(req.GetWorkingDirectory())
	if err != nil {
		t.Fatal(err)
	}
	if stdout != "outbar\n"+wantDir+"\n" || stderr != "err" {
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

// pidRunning reports whether pid is a live process. A zombie that nothing
// reaped counts as dead, because the kill already took effect.
func pidRunning(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	if stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat"); err == nil {
		if i := strings.LastIndexByte(string(stat), ')'); i >= 0 && strings.HasPrefix(strings.TrimSpace(string(stat)[i+1:]), "Z") {
			return false
		}
	}
	return true
}

// readPidFile waits for the fixture to write the pid of its background child.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

func TestExecTimeoutKillsGroupAfterParentExits(t *testing.T) {
	h := newHarness(t, nil)
	grace := time.Second
	h.srv.execs.mu.Lock()
	h.srv.execs.killGrace = grace
	h.srv.execs.mu.Unlock()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The leader exits on SIGTERM while its background child ignores SIGTERM.
	// The child has no output pipe, so cmd.Wait returns when the leader exits.
	script := `trap 'exit 0' TERM
sh -c 'trap "" TERM; echo $$ > "$1"; while :; do sleep 0.05; done' sh "$PID_FILE" >/dev/null 2>&1 &
while :; do sleep 0.05; done`
	req := execReq(t, "/bin/sh", "-c", script)
	req.Env = map[string]string{"PID_FILE": pidFile}
	req.Timeout = durationpb.New(500 * time.Millisecond)
	evs := h.runExec(req)
	childPid := readPidFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(childPid, syscall.SIGKILL) })
	if err := requireExplicitZeroExit(evs[len(evs)-1], true); err != nil {
		t.Fatal(err)
	}
	// What the consumer sees of the child depends on its own scheduling, not
	// on the order of the producer: a slow consumer can read the exited event
	// after the grace period ended and the child is already dead. The
	// ordering of the event and the SIGKILL is covered with an injected timer
	// by TestExecTimeoutEscalationKillsGroupOnlyAfterGraceTimer. This test checks only
	// that the escalation still kills a group that survives its leader.
	deadline := time.Now().Add(grace + 5*time.Second)
	for pidRunning(childPid) {
		if time.Now().After(deadline) {
			t.Fatal("child ignoring SIGTERM survived the grace period")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A pending SIGKILL must not outlive its process group: once every member is
// gone the group id can be reused by an unrelated process group, so the
// escalation ends at that point and not at the end of the grace period.
func TestExecTimeoutDropsKillWhenGroupEmpties(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.execs.mu.Lock()
	h.srv.execs.killGrace = 30 * time.Second
	h.srv.execs.groupPoll = 20 * time.Millisecond
	h.srv.execs.mu.Unlock()
	// The leader exits on SIGTERM. Its background child ignores SIGTERM, as
	// does the sleep it runs, and ends by itself 0.4 seconds later.
	script := `trap 'exit 0' TERM
sh -c 'trap "" TERM; sleep 0.4' >/dev/null 2>&1 &
while :; do sleep 0.05; done`
	req := execReq(t, "/bin/sh", "-c", script)
	req.Timeout = durationpb.New(200 * time.Millisecond)
	evs := h.runExec(req)
	if ex := evs[len(evs)-1].GetExited(); !ex.GetTimedOut() {
		t.Fatalf("exited %v", ex)
	}
	done := make(chan struct{})
	go func() {
		h.srv.execs.escalations.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the pending SIGKILL outlived its empty process group")
	}
}

func TestExecNoKillWhenCommandEndsBeforeTimeout(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.execs.mu.Lock()
	h.srv.execs.killGrace = 100 * time.Millisecond
	h.srv.execs.mu.Unlock()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	req := execReq(t, "/bin/sh", "-c", `sleep 30 >/dev/null 2>&1 & echo $! > "$PID_FILE"`)
	req.Env = map[string]string{"PID_FILE": pidFile}
	req.Timeout = durationpb.New(200 * time.Millisecond)
	evs := h.runExec(req)
	childPid := readPidFile(t, pidFile)
	t.Cleanup(func() { _ = syscall.Kill(childPid, syscall.SIGKILL) })
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatal(err)
	}
	// Neither the timeout nor the escalation may run for a finished command.
	time.Sleep(time.Second)
	if !pidRunning(childPid) {
		t.Fatal("a command that ended before its timeout had its process group killed")
	}
}

func TestExecNoTimeoutWhenDescendantHoldsPipeAfterExit(t *testing.T) {
	h := newHarness(t, nil)
	h.srv.execs.mu.Lock()
	h.srv.execs.killGrace = 100 * time.Millisecond
	h.srv.execs.mu.Unlock()
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	// The descendant inherits stdout, so cmd.Wait drains the pipe for
	// pipeWaitDelay after the foreground shell exited, across the timeout.
	req := execReq(t, "/bin/sh", "-c", `sleep 30 & echo $! > "$PID_FILE"`)
	req.Env = map[string]string{"PID_FILE": pidFile}
	req.Timeout = durationpb.New(300 * time.Millisecond)
	// Registered before the run so the descendant dies even on a failure.
	t.Cleanup(func() {
		if b, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})
	evs := h.runExec(req)
	childPid := readPidFile(t, pidFile)
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if !pidRunning(childPid) {
		t.Fatal("a command that exited before its timeout had its process group signalled")
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
	if stdout != "lloworld" {
		t.Fatalf("attach events %v", evs)
	}
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatalf("attach events %v: %v", evs, err)
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

// TestExecKeepalive checks that a silent exec produces a keepalive event. The
// command stays alive until the test sees a keepalive after the start event,
// or until cleanup releases it, so a delayed Serve reader cannot observe an
// exec that already ended without any idle time.
func TestExecKeepalive(t *testing.T) {
	h := newHarness(t, func(c *Config) { c.KeepaliveInterval = 50 * time.Millisecond })
	release := filepath.Join(t.TempDir(), "release")
	// Registered after newHarness, so it runs before the harness cleanup that
	// waits for the exec: a failing test never leaves the fixture running.
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })
	req := execReq(t, "/bin/sh", "-c", `while [ ! -e "$RELEASE" ]; do sleep 0.02; done`)
	req.Env = map[string]string{"RELEASE": release}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s := h.open(ctx)
	s.send(1, req)

	started := false
	for {
		f := s.recv()
		if f.GetError() != nil {
			t.Fatalf("error frame: %v", f.GetError())
		}
		ev := f.GetExecEvent()
		switch ev.GetEvent().(type) {
		case *ownsandv1.ExecEvent_Started:
			started = true
		case *ownsandv1.ExecEvent_Keepalive:
			if !started {
				// The log records the start event only after the process
				// spawned, so a slow spawn legitimately gets keepalives
				// first. Only a keepalive after the start event counts.
				continue
			}
			if err := os.WriteFile(release, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			// The command is released: it must now exit successfully.
			for {
				ev := s.recv().GetExecEvent()
				switch ev.GetEvent().(type) {
				case *ownsandv1.ExecEvent_Failed:
					t.Fatalf("exec failed after release: %v", ev.GetFailed())
				case *ownsandv1.ExecEvent_Exited:
					if err := requireExplicitSuccess(ev); err != nil {
						t.Fatalf("exec after release: %v", err)
					}
					return
				}
			}
		case *ownsandv1.ExecEvent_Failed:
			t.Fatalf("exec failed before a keepalive: %v", ev.GetFailed())
		case *ownsandv1.ExecEvent_Exited:
			t.Fatal("command exited before a keepalive although it was held open")
		}
	}
}

func TestCommandEnvOverridesHelperEnv(t *testing.T) {
	t.Setenv("OWNSAND_GUEST_TEST_VAR", "helper")
	env := strings.Join(commandEnv(map[string]string{"OWNSAND_GUEST_TEST_VAR": "request"}), "\n")
	if !strings.Contains(env, "OWNSAND_GUEST_TEST_VAR=request") || strings.Contains(env, "=helper") {
		t.Fatal("request env did not override the helper env")
	}
}

// TestServeOversizeFrameEndsStreamWhileExecRuns checks that a frame above the
// receive limit ends the Serve stream with resource_exhausted at once, also
// while an exec of that stream still runs, and that the exec keeps running.
func TestServeOversizeFrameEndsStreamWhileExecRuns(t *testing.T) {
	h := newHarness(t, nil)
	release := filepath.Join(t.TempDir(), "release")
	// The exec ends only after the test creates the release file, so a Serve
	// that waits for its handlers before it cancels them cannot return
	// within the deadline below.
	req := execReq(t, "/bin/sh", "-c", `printf ready; while [ ! -e "$RELEASE" ]; do sleep 0.02; done; printf done`)
	req.Env = map[string]string{"RELEASE": release}
	t.Cleanup(func() { _ = os.WriteFile(release, nil, 0o600) })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s := h.open(ctx)
	s.send(1, req)
	if f := s.recv(); f.GetExecEvent().GetStarted() == nil {
		t.Fatalf("first frame %v", f)
	}

	// 3 MiB is above the 1 MiB plus 128 KiB receive limit of the handler.
	oversize := datapath.MustNewFrame(2, testSandbox, &ownsandv1.ReadFileRequest{
		SandboxId: testSandbox, ReadId: newID(t), Path: strings.Repeat("a", 3<<20),
	})
	// The server may reset the stream before the whole frame is written, so
	// a send error is not a failure; the receive below decides.
	_ = s.s.Send(&ownsandv1.ServeRequest{Frame: oversize})
	start := time.Now()
	var err error
	for err == nil {
		_, err = s.s.Receive()
	}
	if code := errorCode(err); code != connect.CodeResourceExhausted {
		t.Fatalf("receive error %v (code %v) after %v", err, code, time.Since(start))
	}
	if ctx.Err() != nil {
		t.Fatalf("stream error arrived only when the deadline ended: %v", err)
	}

	// The exec outlives the stream: it is still running, and an attach on a
	// new stream returns the kept output and the result.
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	s2 := h.open(ctx2)
	defer s2.s.CloseRequest()
	s2.send(3, &ownsandv1.AttachExecRequest{SandboxId: testSandbox, ExecId: req.GetExecId()})
	evs := s2.collectExec()
	if stdout, _ := outputs(evs); stdout != "readydone" {
		t.Fatalf("attach events %v", evs)
	}
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatalf("attach events %v: %v", evs, err)
	}
}
