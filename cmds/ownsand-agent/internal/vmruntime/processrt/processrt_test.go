// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package processrt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

var guestBinary string

func TestMain(m *testing.M) {
	os.Exit(run(m))
}

// run builds the guest helper into a temporary directory, runs the tests and
// returns the exit code. The directory is removed on every path, including a
// failed helper build; a panic or os.Exit before the deferred cleanup would
// leak it.
func run(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ownsand-processrt-test-")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create harness temp dir: %v\n", err)
		return 1
	}
	defer os.RemoveAll(dir)
	guestBinary = filepath.Join(dir, "ownsand-guest")
	out, err := exec.Command("go", "build", "-o", guestBinary, "github.com/delinoio/mono/cmds/ownsand-guest").CombinedOutput()
	if err != nil {
		fmt.Fprintf(os.Stderr, "build guest helper: %v\n%s", err, out)
		return 1
	}
	return m.Run()
}

func TestParseProcessList(t *testing.T) {
	got, err := parseProcessList([]byte("    1     0     1\n 4242  4000  4242\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []process{{1, 0, 1}, {4242, 4000, 4242}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseProcessList = %v; want %v", got, want)
	}
	for _, bad := range []string{"1 2\n", "1 2 x\n", "1 2 3 4\n"} {
		if _, err := parseProcessList([]byte(bad)); err == nil {
			t.Errorf("parseProcessList(%q) succeeded; want an error", bad)
		}
	}
}

func TestGroupsBelow(t *testing.T) {
	procs := []process{
		{pid: 10, ppid: 1, pgid: 10},   // helper (root)
		{pid: 11, ppid: 10, pgid: 10},  // helper thread-like child in the helper group
		{pid: 20, ppid: 10, pgid: 20},  // exec worker, own group
		{pid: 21, ppid: 20, pgid: 20},  // child of the worker, same group
		{pid: 22, ppid: 20, pgid: 22},  // grandchild in its own group
		{pid: 30, ppid: 10, pgid: 30},  // second exec worker
		{pid: 40, ppid: 1, pgid: 40},   // unrelated process
		{pid: 41, ppid: 40, pgid: 40},  // unrelated child
		{pid: 50, ppid: 22, pgid: 99},  // below the grandchild, agent group
		{pid: 51, ppid: 22, pgid: 51},  // below the grandchild
		{pid: 60, ppid: 1, pgid: 60},   // detached: parent is init
		{pid: 61, ppid: 999, pgid: 61}, // parent missing from the list
	}
	got := groupsBelow(procs, 10, map[int]bool{0: true, 1: true, 10: true, 99: true})
	want := []int{20, 22, 30, 51}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("groupsBelow = %v; want %v", got, want)
	}
}

// processState returns the `ps` state of pid, or "" when the process does
// not exist. A zombie reports "Z".
func processState(t *testing.T, pid int) string {
	t.Helper()
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// waitState waits until the state of pid starts with one of the letters in
// want ("" in want means the process is gone or a zombie).
func waitState(t *testing.T, what string, pid int, want ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var state string
	for {
		state = processState(t, pid)
		for _, w := range want {
			if (w == "" && (state == "" || strings.HasPrefix(state, "Z"))) || (w != "" && strings.HasPrefix(state, w)) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (pid %d): state %q; want one of %q", what, pid, state, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startWorker runs `sh -c "sleep 300 & echo $! $$; wait"` as an exec in the
// sandbox. The guest helper starts the shell in a process group of its own,
// so neither the shell nor the sleep process is in the helper's group. It
// returns the process IDs of the shell and of the sleep process.
func startWorker(t *testing.T, v vmruntime.VM) (shell, sleeper int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client := guestclient.New(v.Dial)
	t.Cleanup(client.Close)
	execID, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	s := client.Serve(ctx)
	t.Cleanup(func() { _ = s.CloseResponse() })
	req := &ownsandv1.ExecRequest{
		OrganizationId: "0192f4b6-7c3a-7def-8abc-0123456789ab",
		SandboxId:      v.SandboxID(),
		ExecId:         execID,
		Command:        []string{"sh", "-c", "sleep 300 & echo $! $$; wait"},
		BindingSalt:    make([]byte, 32),
	}
	if err := s.Send(&ownsandv1.ServeRequest{Frame: datapath.MustNewFrame(1, v.SandboxID(), req)}); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	for {
		resp, err := s.Receive()
		if err != nil {
			t.Fatalf("exec stream: %v (output %q)", err, out.String())
		}
		ev := resp.GetFrame().GetExecEvent()
		if f := ev.GetFailed(); f != nil {
			t.Fatalf("exec failed: %v", f)
		}
		out.Write(ev.GetStdout().GetData())
		if fields := strings.Fields(out.String()); len(fields) == 2 && strings.HasSuffix(out.String(), "\n") {
			sleeper, err = strconv.Atoi(fields[0])
			if err != nil {
				t.Fatal(err)
			}
			shell, err = strconv.Atoi(fields[1])
			if err != nil {
				t.Fatal(err)
			}
			return shell, sleeper
		}
	}
}

// longSandboxDir returns a sandbox directory whose path alone exceeds the
// 104-byte unix socket path limit of macOS, so that a socket inside it could
// not be bound there.
func longSandboxDir(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), strings.Repeat("d", maxTestSocketPath), "sb")
	if len(dir) <= maxTestSocketPath {
		t.Fatalf("sandbox directory %s is not longer than %d bytes", dir, maxTestSocketPath)
	}
	return dir
}

// maxTestSocketPath is the longest unix socket path that macOS accepts.
const maxTestSocketPath = 103

func newRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(guestBinary, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	closeRuntimeOnCleanup(t, rt)
	return rt
}

// closeRuntimeOnCleanup closes rt when the test ends and then kills every
// helper that Close left alive.
//
// Close ends with an error, and leaves the helper alive and stopped, when the
// helper is not observed stopped (see stopHelper). Every helper writes to the
// stderr of the test binary, so one that outlives the test keeps the pipe of
// `go test` open: go test prints PASS and then waits the 1m WaitDelay before it
// reports "Test I/O incomplete" and fails the package. The sweep makes sure
// that no helper survives a test, and the error makes the test fail instead of
// leaving the leak to the next run.
func closeRuntimeOnCleanup(t *testing.T, rt *Runtime) {
	t.Helper()
	// Cleanups run last to first: register the sweep first so that it runs
	// after Close.
	t.Cleanup(func() {
		rt.mu.Lock()
		leftover := make([]*vm, 0, len(rt.vms))
		for _, v := range rt.vms {
			leftover = append(leftover, v)
		}
		rt.mu.Unlock()
		for _, v := range leftover {
			if v.hasExited() {
				continue
			}
			t.Errorf("helper of sandbox %s outlived Close; killing it", v.id)
			_ = v.helper.signal(sigKill)
			select {
			case <-v.exited:
			case <-time.After(10 * time.Second):
				t.Errorf("helper of sandbox %s did not exit after SIGKILL", v.id)
			}
		}
	})
	t.Cleanup(func() {
		if err := rt.Close(); err != nil {
			t.Errorf("close the runtime: %v", err)
		}
	})
}

// useStopSeams installs observe as the observation of the helper's state and
// wait as the wait for the helper to stop (see vm.observeStopped and
// vm.stopWait), and removes both when the test ends. The cleanup of the
// runtime stops the helper through the same fields, so a seam that stayed
// installed would make that stop wait for a helper that never reports stopped,
// give up with the helper stopped and alive, and leak it.
func useStopSeams(t *testing.T, h *vm, observe func(context.Context, int) (bool, error), wait time.Duration) {
	t.Helper()
	h.observeStopped, h.stopWait = observe, wait
	t.Cleanup(func() { h.observeStopped, h.stopWait = nil, 0 })
}

// TestRelativeGuestBinaryResolvesAgainstAgentDirectory checks that a relative
// helper path is validated and launched against the same base, although the
// helper starts with the sandbox root as its working directory.
func TestRelativeGuestBinaryResolvesAgainstAgentDirectory(t *testing.T) {
	data, err := os.ReadFile(guestBinary)
	if err != nil {
		t.Fatal(err)
	}
	agentDir := t.TempDir()
	if err := os.Mkdir(filepath.Join(agentDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "bin", "ownsand-guest"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(agentDir)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	if _, err := New(filepath.Join("bin", "missing-guest"), logger); err == nil {
		t.Fatal("New accepted a missing relative helper")
	}
	rt, err := New(filepath.Join("bin", "ownsand-guest"), logger)
	if err != nil {
		t.Fatal(err)
	}
	closeRuntimeOnCleanup(t, rt)
	v := createSandbox(t, rt)
	if err := rt.Delete(context.Background(), v.SandboxID()); err != nil {
		t.Fatal(err)
	}
}

func createSandbox(t *testing.T, rt *Runtime) vmruntime.VM {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.Create(context.Background(), vmruntime.Spec{
		SandboxID: id,
		Dir:       longSandboxDir(t),
		GuestOS:   hostGuestOS,
		BaseImage: hostBaseImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// TestLifecycleSignalsReachExecWorkers checks that pause, resume and delete
// of one sandbox stop, continue and kill the exec worker processes of that
// sandbox, which run in process groups of their own, and leave the worker of
// a second sandbox alone.
func TestLifecycleSignalsReachExecWorkers(t *testing.T) {
	rt := newRuntime(t)
	a := createSandbox(t, rt)
	b := createSandbox(t, rt)
	aShell, aSleep := startWorker(t, a)
	bShell, bSleep := startWorker(t, b)

	// The workers are not in the helper's process group; this is the
	// condition that the helper-group signal missed.
	helper, err := rt.lookup(a.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	aHelper := helper.helper.pid
	for _, pid := range []int{aShell, aSleep} {
		pgid, err := syscall.Getpgid(pid)
		if err != nil || pgid == aHelper {
			t.Fatalf("worker pid %d: process group %d, %v; want a group other than the helper group %d", pid, pgid, err, aHelper)
		}
	}
	running := func(what string, pids ...int) {
		t.Helper()
		for _, pid := range pids {
			waitState(t, what, pid, "S", "R")
		}
	}
	running("initial worker A", aShell, aSleep)
	running("initial worker B", bShell, bSleep)

	if err := rt.Pause(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{aShell, aSleep} {
		waitState(t, "paused worker A", pid, "T")
	}
	running("worker B while A is paused", bShell, bSleep)

	if err := rt.Resume(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	running("resumed worker A", aShell, aSleep)
	running("worker B after A resumed", bShell, bSleep)

	// Delete a paused sandbox: the workers must die although they are
	// stopped.
	if err := rt.Pause(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if err := rt.Delete(context.Background(), a.SandboxID()); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{aShell, aSleep} {
		// A killed process can stay a zombie until its adopter reaps it.
		waitState(t, "deleted worker A", pid, "")
	}
	running("worker B after A deleted", bShell, bSleep)

	if err := rt.Delete(context.Background(), b.SandboxID()); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{bShell, bSleep} {
		waitState(t, "deleted worker B", pid, "")
	}
}

// TestDeleteRunningSandboxKillsExecWorkers covers Delete without a prior
// pause.
func TestDeleteRunningSandboxKillsExecWorkers(t *testing.T) {
	rt := newRuntime(t)
	v := createSandbox(t, rt)
	shell, sleeper := startWorker(t, v)
	if err := rt.Delete(context.Background(), v.SandboxID()); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "deleted worker", pid, "")
	}
}

// TestDeleteNeverContinuesHelperBeforeKill checks the order of the signals
// that Delete sends to the helper of a running and of a paused sandbox: the
// helper is stopped and then killed, and never continued in between. A
// continued helper can handle a request that it received before the stop and
// start an exec worker after Delete enumerated the workers, and nothing kills
// that worker. The race is too short to hit reliably with a real exec, so the
// test asserts the order of the signals, and still checks that the workers
// that already run die.
func TestDeleteNeverContinuesHelperBeforeKill(t *testing.T) {
	for _, paused := range []bool{false, true} {
		name := "running"
		if paused {
			name = "paused"
		}
		t.Run(name, func(t *testing.T) {
			rt := newRuntime(t)
			m := createSandbox(t, rt)
			shell, sleeper := startWorker(t, m)
			h, err := rt.lookup(m.SandboxID())
			if err != nil {
				t.Fatal(err)
			}
			helperPID := h.helper.pid
			if paused {
				if err := rt.Pause(context.Background(), m); err != nil {
					t.Fatal(err)
				}
			}
			var signals []syscall.Signal
			h.onSignal = func(sig syscall.Signal) { signals = append(signals, sig) }
			if err := rt.Delete(context.Background(), m.SandboxID()); err != nil {
				t.Fatal(err)
			}
			if len(signals) == 0 || signals[len(signals)-1] != syscall.SIGKILL {
				t.Fatalf("signals to the helper during Delete = %v; want them to end with SIGKILL", signals)
			}
			for _, sig := range signals {
				if sig == syscall.SIGCONT {
					t.Fatalf("signals to the helper during Delete = %v; want no SIGCONT before the SIGKILL", signals)
				}
			}
			for _, pid := range []int{shell, sleeper, helperPID} {
				waitState(t, "process after Delete", pid, "")
			}
		})
	}
}

// TestPauseFailureContinuesHelper checks that a Pause that fails while it
// lists the exec process groups (a canceled context fails the `ps` command)
// continues the helper again. The sandbox manager keeps the sandbox in the
// running state when Pause fails, and a later Resume of that state does not
// reach the runtime, so a helper that stayed stopped would freeze the sandbox
// for good.
func TestPauseFailureContinuesHelper(t *testing.T) {
	rt := newRuntime(t)
	v := createSandbox(t, rt)
	shell, sleeper := startWorker(t, v)
	helper, err := rt.lookup(v.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	helperPID := helper.helper.pid

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Pause(canceled, v); err == nil {
		t.Fatal("Pause with a canceled context succeeded; want an error")
	}
	waitState(t, "helper after the failed Pause", helperPID, "S", "R")
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the failed Pause", pid, "S", "R")
	}

	// The next Pause works, which shows that the sandbox is not frozen.
	if err := rt.Pause(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	waitState(t, "helper after the second Pause", helperPID, "T")
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the second Pause", pid, "T")
	}
	if err := rt.Resume(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	waitState(t, "helper after Resume", helperPID, "S", "R")
}

// TestResumeFailureKeepsSandboxStopped checks that a Resume that fails while
// it lists the exec process groups (a canceled context fails the `ps`
// command) leaves the helper and the workers stopped. The sandbox manager
// keeps the sandbox in the paused state when Resume fails, so a helper that
// ran again would be recorded as paused, and its exec timeouts could kill the
// stopped workers. A retry of Resume must continue the whole sandbox.
func TestResumeFailureKeepsSandboxStopped(t *testing.T) {
	rt := newRuntime(t)
	v := createSandbox(t, rt)
	shell, sleeper := startWorker(t, v)
	helper, err := rt.lookup(v.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	helperPID := helper.helper.pid

	if err := rt.Pause(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Resume(canceled, v); err == nil {
		t.Fatal("Resume with a canceled context succeeded; want an error")
	}
	waitState(t, "helper after the failed Resume", helperPID, "T")
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the failed Resume", pid, "T")
	}

	// The retry continues the helper and the workers.
	if err := rt.Resume(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	waitState(t, "helper after the retried Resume", helperPID, "S", "R")
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the retried Resume", pid, "S", "R")
	}
}

func TestParseProcStatState(t *testing.T) {
	for _, tc := range []struct {
		name string
		data string
		want byte
	}{
		{"running", "42 (ownsand-guest) S 1 42 42 0 -1", 'S'},
		{"stopped", "42 (ownsand-guest) T 1 42 42 0 -1\n", 'T'},
		{"name with spaces and parentheses", "42 (a b) T) T 1 42", 'T'},
		{"zombie", "42 (x) Z 1 42", 'Z'},
	} {
		got, err := parseProcStatState([]byte(tc.data))
		if err != nil || got != tc.want {
			t.Errorf("%s: parseProcStatState = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", "42 x S 1", "42 (x)", "42 (x) ST 1"} {
		if _, err := parseProcStatState([]byte(bad)); err == nil {
			t.Errorf("parseProcStatState(%q) succeeded; want an error", bad)
		}
	}
}

func TestStateIsStopped(t *testing.T) {
	for _, state := range []byte("TtZXx") {
		if !stateIsStopped(state) {
			t.Errorf("stateIsStopped(%q) = false; want true", state)
		}
	}
	for _, state := range []byte("SRDIW") {
		if stateIsStopped(state) {
			t.Errorf("stateIsStopped(%q) = true; want false", state)
		}
	}
}

func TestPSStopped(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		stopped bool
		wantErr bool
	}{
		{name: "absent", script: "exit 1", stopped: true},
		{name: "stopped", script: "printf ' T+\n'", stopped: true},
		{name: "dead", script: "printf 'Z\n'", stopped: true},
		{name: "running", script: "printf 'R\n'"},
		{name: "exit 2", script: "exit 2", wantErr: true},
		{name: "terminated", script: "kill -KILL $$", wantErr: true},
		{name: "diagnostic", script: "printf 'cannot read process state\n' >&2; exit 1", wantErr: true},
		{name: "exit 1 with state", script: "printf 'T\n'; exit 1", wantErr: true},
		{name: "empty success", script: "exit 0", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installPSFixture(t, tt.script)
			stopped, err := psStopped(context.Background(), os.Getpid())
			if stopped != tt.stopped || (err != nil) != tt.wantErr {
				t.Fatalf("psStopped = %v, %v; want stopped=%v, error=%v", stopped, err, tt.stopped, tt.wantErr)
			}
		})
	}
	t.Run("missing executable", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if stopped, err := psStopped(context.Background(), os.Getpid()); stopped || !errors.Is(err, exec.ErrNotFound) {
			t.Fatalf("psStopped = %v, %v; want false, exec.ErrNotFound", stopped, err)
		}
	})
	t.Run("canceled context", func(t *testing.T) {
		installPSFixture(t, "exit 1")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if stopped, err := psStopped(ctx, os.Getpid()); stopped || !errors.Is(err, context.Canceled) {
			t.Fatalf("psStopped = %v, %v; want false, context.Canceled", stopped, err)
		}
	})
}

func installPSFixture(t *testing.T, script string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ps"), []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestHelperStoppedObservesRealHelper checks the observation of a real helper
// process through both the /proc reader and the `ps` reader: a running helper
// is not stopped, a SIGSTOP stops it, a SIGCONT runs it again, and a reaped
// process counts as stopped. On macOS only the `ps` reader runs.
func TestHelperStoppedObservesRealHelper(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	h, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	pid := h.helper.pid
	readers := map[string]func() (bool, error){
		"ps": func() (bool, error) { return psStopped(context.Background(), pid) },
	}
	if runtime.GOOS == "linux" {
		readers["proc"] = func() (bool, error) { return procThreadsStopped(pid) }
		readers["helperStopped"] = func() (bool, error) { return helperStopped(context.Background(), pid) }
	}
	wait := func(name string, want bool, read func() (bool, error)) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			got, err := read()
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if got == want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: stopped = %v; want %v", name, got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	for name, read := range readers {
		wait(name+" running helper", false, read)
	}
	if err := h.signalHelper(sigStop); err != nil {
		t.Fatal(err)
	}
	for name, read := range readers {
		wait(name+" stopped helper", true, read)
	}
	if err := h.signalHelper(sigCont); err != nil {
		t.Fatal(err)
	}
	for name, read := range readers {
		wait(name+" continued helper", false, read)
	}
	if err := rt.Delete(context.Background(), m.SandboxID()); err != nil {
		t.Fatal(err)
	}
	for name, read := range readers {
		wait(name+" deleted helper", true, read)
	}
}

// countingObserver returns an observer that reports the helper as not
// stopped for the first falseCalls calls and then delegates to the real
// observation, and a function that returns the number of calls. onFalse runs
// before each call that reports not stopped.
func countingObserver(falseCalls int, onFalse func(call int)) (func(context.Context, int) (bool, error), func() int) {
	var calls atomic.Int32
	observe := func(ctx context.Context, pid int) (bool, error) {
		n := int(calls.Add(1))
		if n <= falseCalls {
			if onFalse != nil {
				onFalse(n)
			}
			return false, nil
		}
		return helperStopped(ctx, pid)
	}
	return observe, func() int { return int(calls.Load()) }
}

// TestPauseEnumeratesOnlyAfterHelperIsObservedStopped checks that Pause does
// not stop the exec workers before it observes the helper stopped: while the
// observation still reports a running helper, the worker keeps running, and
// the worker stops only after the observation succeeded.
func TestPauseEnumeratesOnlyAfterHelperIsObservedStopped(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	shell, sleeper := startWorker(t, m)
	h, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	observe, calls := countingObserver(3, func(call int) {
		for _, pid := range []int{shell, sleeper} {
			if state := processState(t, pid); strings.HasPrefix(state, "T") {
				t.Errorf("worker %d stopped (state %q) before the helper was observed stopped (call %d)", pid, state, call)
			}
		}
	})
	useStopSeams(t, h, observe, 0)
	if err := rt.Pause(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if got := calls(); got != 4 {
		t.Errorf("observations of the helper = %d; want 4 (three not stopped, one stopped)", got)
	}
	for _, pid := range []int{h.helper.pid, shell, sleeper} {
		waitState(t, "process after Pause", pid, "T")
	}
	useStopSeams(t, h, nil, 0)
	if err := rt.Resume(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	waitState(t, "worker after Resume", shell, "S", "R")
}

// TestPauseRollsBackWhenHelperDoesNotStop checks that a Pause whose helper is
// never observed stopped fails, signals no worker, and continues the helper
// again, so that the sandbox is not frozen.
func TestPauseRollsBackWhenHelperDoesNotStop(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	shell, sleeper := startWorker(t, m)
	h, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	helperPID := h.helper.pid
	var signals []syscall.Signal
	h.onSignal = func(sig syscall.Signal) { signals = append(signals, sig) }
	useStopSeams(t, h, func(context.Context, int) (bool, error) { return false, nil }, 100*time.Millisecond)

	err = rt.Pause(context.Background(), m)
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Pause = %v; want a did-not-stop error", err)
	}
	if want := []syscall.Signal{syscall.SIGSTOP, syscall.SIGCONT}; !reflect.DeepEqual(signals, want) {
		t.Errorf("signals to the helper = %v; want %v", signals, want)
	}
	waitState(t, "helper after the failed Pause", helperPID, "S", "R")
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the failed Pause", pid, "S", "R")
	}

	// A later Pause works once the helper is observed stopped.
	useStopSeams(t, h, nil, 0)
	if err := rt.Pause(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{helperPID, shell, sleeper} {
		waitState(t, "process after the second Pause", pid, "T")
	}
}

// TestDeleteFailsWhenHelperDoesNotStop checks that a Delete whose helper is
// never observed stopped returns an error, kills neither the workers nor the
// helper, keeps the sandbox tracked and reports no cleanup, and that a retry
// completes the cleanup.
func TestDeleteFailsWhenHelperDoesNotStop(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	shell, sleeper := startWorker(t, m)
	h, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	helperPID := h.helper.pid
	var signals []syscall.Signal
	h.onSignal = func(sig syscall.Signal) { signals = append(signals, sig) }
	useStopSeams(t, h, func(context.Context, int) (bool, error) { return false, nil }, 100*time.Millisecond)

	err = rt.Delete(context.Background(), m.SandboxID())
	if err == nil || !strings.Contains(err.Error(), "did not stop") {
		t.Fatalf("Delete = %v; want a did-not-stop error", err)
	}
	for _, sig := range signals {
		if sig == syscall.SIGKILL {
			t.Fatalf("signals to the helper = %v; want no SIGKILL while the helper is not observed stopped", signals)
		}
	}
	if _, err := rt.lookup(m.SandboxID()); err != nil {
		t.Fatalf("sandbox after the failed Delete: %v; want it tracked", err)
	}
	if _, err := os.Stat(h.dir); err != nil {
		t.Errorf("sandbox directory after the failed Delete: %v; want it kept", err)
	}
	if h.hasExited() {
		t.Fatal("the helper died although it was not observed stopped")
	}
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the failed Delete", pid, "S", "R")
	}

	useStopSeams(t, h, nil, 0)
	if err := rt.Delete(context.Background(), m.SandboxID()); err != nil {
		t.Fatalf("retried Delete: %v", err)
	}
	for _, pid := range []int{shell, sleeper, helperPID} {
		waitState(t, "process after the retried Delete", pid, "")
	}
	if _, err := rt.lookup(m.SandboxID()); err == nil {
		t.Error("sandbox is still tracked after the retried Delete")
	}
}

// TestLifecycleEnumeratesOnlyAfterHelperPSStopped uses the real helper and
// exec workers. The observer runs psStopped on Linux as well as on macOS.
func TestLifecycleEnumeratesOnlyAfterHelperPSStopped(t *testing.T) {
	operations := []struct {
		name string
		run  func(*Runtime, vmruntime.VM) error
	}{
		{name: "Pause", run: func(rt *Runtime, m vmruntime.VM) error { return rt.Pause(context.Background(), m) }},
		{name: "Delete", run: func(rt *Runtime, m vmruntime.VM) error { return rt.Delete(context.Background(), m.SandboxID()) }},
	}
	for _, op := range operations {
		for _, failure := range []string{"persistent", "transient"} {
			t.Run(op.name+"/"+failure, func(t *testing.T) {
				rt := newRuntime(t)
				m := createSandbox(t, rt)
				shell, sleeper := startWorker(t, m)
				h, err := rt.lookup(m.SandboxID())
				if err != nil {
					t.Fatal(err)
				}
				realPS, err := exec.LookPath("ps")
				if err != nil {
					t.Fatal(err)
				}
				oldPath := os.Getenv("PATH")
				listed := filepath.Join(t.TempDir(), "listed")
				t.Setenv("PROCESSRT_TEST_PS", realPS)
				t.Setenv("PROCESSRT_TEST_PS_LISTED", listed)
				t.Setenv("PROCESSRT_TEST_PS_FAILURE", failure)
				t.Setenv("PROCESSRT_TEST_PS_FAILED", filepath.Join(t.TempDir(), "failed"))
				installPSFixture(t, `
if [ "$1" = "-A" ]; then
    touch "$PROCESSRT_TEST_PS_LISTED" || exit 3
elif [ "$PROCESSRT_TEST_PS_FAILURE" = "persistent" ] || [ ! -e "$PROCESSRT_TEST_PS_FAILED" ]; then
    touch "$PROCESSRT_TEST_PS_FAILED" || exit 3
    exit 2
fi
exec "$PROCESSRT_TEST_PS" "$@"
`)
				observations := 0
				observedStopped := false
				wait := 100 * time.Millisecond
				if failure == "transient" {
					wait = 0 // Use the real timeout while waiting for the stop signal to take effect.
				}
				useStopSeams(t, h, func(ctx context.Context, pid int) (bool, error) {
					mustNotExist(t, listed)
					stopped, err := psStopped(ctx, pid)
					observations++
					if observations == 1 {
						var exitErr *exec.ExitError
						if stopped || !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
							t.Errorf("first observation = %v, %v; want false and ps exit 2", stopped, err)
						}
					}
					observedStopped = stopped && err == nil
					return stopped, err
				}, wait)

				err = op.run(rt, m)
				t.Setenv("PATH", oldPath)
				if failure == "persistent" {
					var exitErr *exec.ExitError
					if !errors.As(err, &exitErr) || exitErr.ExitCode() != 2 {
						t.Fatalf("%s = %v; want an error from the failed ps probe", op.name, err)
					}
					mustNotExist(t, listed)
					if _, err := rt.lookup(m.SandboxID()); err != nil {
						t.Fatalf("sandbox after failed %s: %v", op.name, err)
					}
					mustExist(t, h.dir)
					if h.hasExited() {
						t.Fatal("the helper exited while the stop probe was failing")
					}
					if op.name == "Pause" {
						waitState(t, "helper after failed Pause", h.helper.pid, "S", "R")
					}
					for _, pid := range []int{shell, sleeper} {
						waitState(t, "worker after failed "+op.name, pid, "S", "R")
					}
					useStopSeams(t, h, nil, 0)
					if err := op.run(rt, m); err != nil {
						t.Fatalf("retried %s: %v", op.name, err)
					}
					return
				}

				if err != nil {
					t.Fatal(err)
				}
				if observations < 2 || !observedStopped {
					t.Fatalf("%s listed workers without a valid stop after the failed probe", op.name)
				}
				mustExist(t, listed)
			})
		}
	}
}

// TestWaitHelperStoppedExitedHelper checks that a reaped helper counts as
// stopped without any observation, because the observation would address a
// process ID that another process can hold.
func TestWaitHelperStoppedExitedHelper(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	h := killHelperAndWait(t, rt, m)
	useStopSeams(t, h, func(context.Context, int) (bool, error) {
		t.Error("observed a reaped helper")
		return false, nil
	}, 0)
	start := time.Now()
	if err := h.waitHelperStopped(context.Background()); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait for a reaped helper took %s; want it to return at once", elapsed)
	}
}

// TestWaitHelperStoppedEndsWhenHelperExitsDuringWait checks that a helper
// that exits while the runtime waits for its stop ends the wait without an
// error.
func TestWaitHelperStoppedEndsWhenHelperExitsDuringWait(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	h, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	// Kill the helper once, from the first observation: the helper cannot be
	// reaped before it was killed, so the process ID is still its own.
	var kill sync.Once
	useStopSeams(t, h, func(context.Context, int) (bool, error) {
		kill.Do(func() { go func() { _ = syscall.Kill(h.helper.pid, syscall.SIGKILL) }() })
		return false, nil
	}, 10*time.Second)
	if err := h.waitHelperStopped(context.Background()); err != nil {
		t.Fatalf("wait for a helper that exits: %v", err)
	}
	if !h.hasExited() {
		t.Error("wait ended before the helper exited")
	}
}

// TestWaitHelperStoppedReportsObservationError checks that an observation
// that keeps failing ends in a timeout error that names the cause, and that a
// canceled context ends the wait at once.
func TestWaitHelperStoppedReportsObservationError(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	h, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	cause := errors.New("ps is broken")
	useStopSeams(t, h, func(context.Context, int) (bool, error) { return false, cause }, 50*time.Millisecond)
	if err := h.waitHelperStopped(context.Background()); !errors.Is(err, cause) {
		t.Errorf("wait with a failing observation = %v; want it to wrap %v", err, cause)
	}

	useStopSeams(t, h, h.observeStopped, 10*time.Second)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	if err := h.waitHelperStopped(canceled); !errors.Is(err, context.Canceled) {
		t.Errorf("wait with a canceled context = %v; want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("wait with a canceled context took %s; want it to return at once", elapsed)
	}
}

// killHelperAndWait kills the helper of v by its positive process ID, so that
// only the helper dies, and waits until the runtime has reaped it. It returns
// the helper.
func killHelperAndWait(t *testing.T, rt *Runtime, v vmruntime.VM) *vm {
	t.Helper()
	h, err := rt.lookup(v.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(h.helper.pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	select {
	case <-v.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the helper did not exit")
	}
	return h
}

// bystanderAsReusedGroup starts an unrelated process that leads a process
// group of its own and makes h address that group, as if the operating system
// gave the process ID of the reaped helper to it. It returns the bystander's
// process ID. A signal that the runtime still sends to the helper's group
// reaches the bystander.
func bystanderAsReusedGroup(t *testing.T, h *vm) int {
	t.Helper()
	cmd := exec.Command("sleep", "300")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	h.helper = helperProcess{
		pid:    cmd.Process.Pid,
		signal: func(sig syscall.Signal) error { return syscall.Kill(-cmd.Process.Pid, sig) },
	}
	waitState(t, "bystander", cmd.Process.Pid, "S", "R")
	return cmd.Process.Pid
}

// TestExitedHelperReceivesNoSignal checks that Pause, Resume and Delete send no
// signal to the process group of a helper that was reaped, because the
// operating system may have given that process ID to another process group,
// and that they report the exited helper instead of a raw ESRCH.
func TestExitedHelperReceivesNoSignal(t *testing.T) {
	rt := newRuntime(t)
	v := createSandbox(t, rt)
	h := killHelperAndWait(t, rt, v)
	bystander := bystanderAsReusedGroup(t, h)

	for name, call := range map[string]func() error{
		"Pause":  func() error { return rt.Pause(context.Background(), v) },
		"Resume": func() error { return rt.Resume(context.Background(), v) },
	} {
		err := call()
		if !errors.Is(err, errHelperExited) {
			t.Errorf("%s after the helper exited = %v; want errHelperExited", name, err)
		}
		var errno syscall.Errno
		if errors.As(err, &errno) {
			t.Errorf("%s after the helper exited = %v; want no syscall error", name, err)
		}
		waitState(t, "bystander after "+name, bystander, "S", "R")
	}

	start := time.Now()
	if err := rt.Delete(context.Background(), v.SandboxID()); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("Delete of an exited helper took %v; want no wait for the helper", d)
	}
	waitState(t, "bystander after Delete", bystander, "S", "R")
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Errorf("sandbox directory after Delete: %v; want it removed", err)
	}
	if h.socketDir != "" {
		mustNotExist(t, h.socketDir)
	}
	if _, err := rt.lookup(v.SandboxID()); err == nil {
		t.Error("sandbox is still tracked after Delete")
	}
}

// TestDeleteWithCanceledContextKillsWorkers checks that a Delete whose caller
// context is already canceled still kills the exec workers: the process
// listing that finds them must not use the canceled context, because the
// workers would lose their only handle when the helper dies.
func TestDeleteWithCanceledContextKillsWorkers(t *testing.T) {
	rt := newRuntime(t)
	v := createSandbox(t, rt)
	shell, sleeper := startWorker(t, v)
	h, err := rt.lookup(v.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	helperPID := h.helper.pid

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rt.Delete(canceled, v.SandboxID()); err != nil {
		t.Fatal(err)
	}
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after Delete with a canceled context", pid, "")
	}
	waitState(t, "helper after Delete with a canceled context", helperPID, "")
	if _, err := os.Stat(h.dir); !os.IsNotExist(err) {
		t.Errorf("sandbox directory after Delete: %v; want it removed", err)
	}
}

// TestDeleteRetryAfterListingFailure checks that a Delete that cannot list the
// exec workers returns an error, keeps the sandbox tracked, leaves the helper
// alive, and that a retry kills the workers and the helper.
func TestDeleteRetryAfterListingFailure(t *testing.T) {
	rt := newRuntime(t)
	v := createSandbox(t, rt)
	shell, sleeper := startWorker(t, v)
	h, err := rt.lookup(v.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	helperPID := h.helper.pid

	// An empty PATH makes the `ps` command of the process listing fail. The
	// observation of the stopped helper also runs `ps` on macOS, so it gets the
	// original PATH; without that, the Delete would wait the whole
	// helperStopTimeout for a stop that it cannot observe, and fail before it
	// lists the workers.
	oldPath := os.Getenv("PATH")
	emptyPath := t.TempDir()
	useStopSeams(t, h, func(ctx context.Context, pid int) (bool, error) {
		if err := os.Setenv("PATH", oldPath); err != nil {
			return false, err
		}
		defer os.Setenv("PATH", emptyPath)
		return helperStopped(ctx, pid)
	}, 0)
	if err := os.Setenv("PATH", emptyPath); err != nil {
		t.Fatal(err)
	}
	err = rt.Delete(context.Background(), v.SandboxID())
	if restoreErr := os.Setenv("PATH", oldPath); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("Delete without a working process listing succeeded; want an error")
	}
	if _, err := rt.lookup(v.SandboxID()); err != nil {
		t.Fatalf("sandbox after the failed Delete: %v; want it tracked", err)
	}
	if h.hasExited() {
		t.Fatal("the helper died although the workers were not found")
	}
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the failed Delete", pid, "S", "R", "T")
	}

	useStopSeams(t, h, nil, 0)
	if err := rt.Delete(context.Background(), v.SandboxID()); err != nil {
		t.Fatalf("retried Delete: %v", err)
	}
	for _, pid := range []int{shell, sleeper} {
		waitState(t, "worker after the retried Delete", pid, "")
	}
	waitState(t, "helper after the retried Delete", helperPID, "")
	if _, err := rt.lookup(v.SandboxID()); err == nil {
		t.Error("sandbox is still tracked after the retried Delete")
	}
}

// TestFailedStartCleanupDoesNotNeedProcessListing checks that a Create whose
// helper does not become ready kills the helper and removes the sandbox even
// when the process listing is unavailable. The caller records a failed create
// as terminal and never deletes the sandbox again, so the runtime must finish
// the cleanup itself. The helper was never handed out, so it has no exec
// workers that the listing would have to find.
func TestFailedStartCleanupDoesNotNeedProcessListing(t *testing.T) {
	rt := newRuntime(t)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	spec := vmruntime.Spec{SandboxID: id, Dir: longSandboxDir(t), GuestOS: hostGuestOS, BaseImage: hostBaseImage}
	// A private, short TMPDIR receives the socket directory, so that the
	// check below does not see the socket directories of other test packages
	// that run at the same time.
	tmp, err := os.MkdirTemp("", "pt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmp) })
	t.Setenv("TMPDIR", tmp)

	// A canceled context fails the readiness wait; an empty PATH makes the
	// `ps` command of the process listing fail.
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	_, err = rt.Create(canceled, spec)
	if restoreErr := os.Setenv("PATH", oldPath); restoreErr != nil {
		t.Fatal(restoreErr)
	}
	if err == nil {
		t.Fatal("Create with a canceled context succeeded; want an error")
	}
	if v, err := rt.lookup(id); err == nil {
		if !v.hasExited() {
			_ = v.helper.signal(sigKill)
		}
		t.Fatal("sandbox is still tracked after the failed Create; the caller cannot retry the cleanup")
	}
	if _, err := os.Stat(spec.Dir); !os.IsNotExist(err) {
		t.Errorf("sandbox directory after the failed Create: %v; want it removed", err)
	}
	if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
		t.Errorf("TMPDIR entries after the failed Create: %v, %v; want none", entries, err)
	}
}

// errAfterContext reports context.Canceled from its Err method after the
// first n calls, so a test cancels the snapshot copy at a fixed entry
// without timing.
type errAfterContext struct {
	context.Context
	left int
}

func (c *errAfterContext) Err() error {
	if c.left <= 0 {
		return context.Canceled
	}
	c.left--
	return nil
}

// snapshotSource writes a snapshot with files disk files and returns it.
func snapshotSource(t *testing.T, files int) *vmruntime.SnapshotSource {
	t.Helper()
	snapDir := t.TempDir()
	disk := filepath.Join(snapDir, diskDir)
	if err := os.MkdirAll(disk, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := range files {
		if err := os.WriteFile(filepath.Join(disk, fmt.Sprintf("f%d", i)), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &vmruntime.SnapshotSource{SnapshotID: "snap", Dir: snapDir}
}

// helperProcessesOf returns the lines of the process listing that mention dir.
func helperProcessesOf(t *testing.T, dir string) []string {
	t.Helper()
	out, err := exec.Command("ps", "-axo", "command").Output()
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.Split(string(out), "\n") {
		if strings.Contains(line, dir) {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestCanceledSnapshotRestoreStartsNoHelper checks that a restore that is
// canceled during the snapshot copy, or right after it, returns the context
// error, removes the partial sandbox directory and starts no helper.
func TestCanceledSnapshotRestoreStartsNoHelper(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files int
		// calls is the number of Err calls that pass before the cancellation.
		calls int
	}{
		// The root entry and two files pass; the third file sees the cancellation.
		{name: "during the copy", files: 20, calls: 4},
		// Only the root entry is copied; the check before the helper start sees it.
		{name: "after the copy", files: 0, calls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := newRuntime(t)
			id, err := uuidv7.NewString()
			if err != nil {
				t.Fatal(err)
			}
			spec := vmruntime.Spec{
				SandboxID: id,
				Dir:       longSandboxDir(t),
				GuestOS:   hostGuestOS,
				BaseImage: hostBaseImage,
				Snapshot:  snapshotSource(t, tc.files),
			}
			// A private, short TMPDIR receives the socket directory. It is
			// set after the fixtures above, which live in the test's own
			// temporary directory.
			tmp, err := os.MkdirTemp("", "pt")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(tmp) })
			t.Setenv("TMPDIR", tmp)
			_, err = rt.Create(&errAfterContext{Context: context.Background(), left: tc.calls}, spec)
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Create error = %v; want context.Canceled", err)
			}
			if _, err := rt.lookup(id); err == nil {
				t.Error("sandbox is tracked after the canceled restore")
			}
			if _, err := os.Stat(spec.Dir); !os.IsNotExist(err) {
				t.Errorf("sandbox directory after the canceled restore: %v; want it removed", err)
			}
			if entries, err := os.ReadDir(tmp); err != nil || len(entries) != 0 {
				t.Errorf("TMPDIR entries after the canceled restore: %v, %v; want none", entries, err)
			}
			if lines := helperProcessesOf(t, spec.Dir); len(lines) != 0 {
				t.Errorf("helper processes after the canceled restore: %v", lines)
			}
		})
	}
}

// TestSnapshotRestoreStartsAndWaitsForGuest checks that an uncanceled restore
// copies the snapshot disk, starts the helper and returns when it is ready.
func TestSnapshotRestoreStartsAndWaitsForGuest(t *testing.T) {
	rt := newRuntime(t)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	spec := vmruntime.Spec{
		SandboxID: id,
		Dir:       longSandboxDir(t),
		GuestOS:   hostGuestOS,
		BaseImage: hostBaseImage,
		Snapshot:  snapshotSource(t, 5),
	}
	if _, err := rt.Create(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(spec.Dir, rootDir, "f4")); err != nil {
		t.Errorf("snapshot file missing from the restored root: %v", err)
	}
	if lines := helperProcessesOf(t, spec.Dir); len(lines) == 0 {
		t.Error("no helper process runs after the restore")
	}
	if err := rt.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}

// TestRelativeSandboxDirectoryKeepsHelperRecordsOutsideRoot checks that a
// sandbox directory given as a relative path (a relative agent state
// directory) gives the helper records (guest-state) the same place as an
// absolute one: next to the sandbox root, not inside it. The helper starts
// with the sandbox root as its working directory, so a relative --state-dir
// would resolve a second time below the root, where Snapshot copies the
// records into the snapshot disk.
func TestRelativeSandboxDirectoryKeepsHelperRecordsOutsideRoot(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(base)
	rt := newRuntime(t)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	relDir := filepath.Join("state", "vms", id)
	m, err := rt.Create(context.Background(), vmruntime.Spec{
		SandboxID: id,
		Dir:       relDir,
		GuestOS:   hostGuestOS,
		BaseImage: hostBaseImage,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Create answered a readiness probe on the socket that Dial uses, so the
	// helper listens where the agent dials.
	c, err := m.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	h, err := rt.lookup(id)
	if err != nil {
		t.Fatal(err)
	}
	absDir := filepath.Join(base, relDir)
	if h.dir != absDir {
		t.Fatalf("sandbox directory of the runtime = %q; want the absolute %q", h.dir, absDir)
	}
	root := filepath.Join(absDir, rootDir)
	if cwd, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", h.helper.pid)); err == nil && cwd != root {
		t.Errorf("helper working directory = %q; want the sandbox root %q", cwd, root)
	}
	// The helper keeps its records beside the root, and nothing named like
	// them exists inside the root.
	records := filepath.Join(absDir, stateDir)
	if st, err := os.Stat(records); err != nil || !st.IsDir() {
		t.Fatalf("helper records %s: %v, %v; want a directory", records, st, err)
	}
	assertNoGuestState := func(what, tree string) {
		t.Helper()
		err := filepath.WalkDir(tree, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.Name() == stateDir {
				t.Errorf("%s contains %s", what, path)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertNoGuestState("the sandbox root", root)

	snapshotDir := t.TempDir()
	if _, err := rt.Snapshot(context.Background(), m, snapshotDir); err != nil {
		t.Fatal(err)
	}
	assertNoGuestState("the snapshot disk", filepath.Join(snapshotDir, diskDir))

	if err := rt.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(absDir); !os.IsNotExist(err) {
		t.Errorf("sandbox directory after Delete: %v; want it removed", err)
	}
	if _, err := os.Stat(filepath.Join("state")); err != nil {
		t.Errorf("parent state directory after Delete: %v; want it kept", err)
	}
}

// TestSocketOutsideLongSandboxDirectory checks that the guest helper serves a
// sandbox whose directory is too long for a unix socket path inside it, and
// that Delete removes the socket directory with the sandbox.
func TestSocketOutsideLongSandboxDirectory(t *testing.T) {
	rt := newRuntime(t)
	m := createSandbox(t, rt)
	v, err := rt.lookup(m.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	if len(v.socket) > maxTestSocketPath {
		t.Fatalf("socket address has %d bytes; want at most %d", len(v.socket), maxTestSocketPath)
	}
	if v.socketDir != "" {
		if st, err := os.Stat(v.socketDir); err != nil || st.Mode().Perm() != 0o700 {
			t.Fatalf("socket directory: %v, %v; want a private directory", st, err)
		}
	}
	c, err := m.Dial(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := rt.Delete(context.Background(), m.SandboxID()); err != nil {
		t.Fatal(err)
	}
	if v.socketDir != "" {
		if _, err := os.Stat(v.socketDir); !os.IsNotExist(err) {
			t.Errorf("socket directory after Delete: %v; want it removed", err)
		}
	}
}

// newRuntimeWithVMsDir returns a runtime that knows vmsDir, as the agent
// builds it, and does not close the helpers on cleanup, so that a test can
// model an earlier agent process whose helpers outlived it.
func newRuntimeWithVMsDir(t *testing.T, vmsDir string) *Runtime {
	t.Helper()
	rt, err := NewWithOptions(Options{GuestBinary: guestBinary, VMsDir: vmsDir, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return rt
}

// writeSandboxFiles writes the layout of a sandbox directory that a process
// runtime leaves behind: root with a user file and guest-state.
func writeSandboxFiles(t *testing.T, vmsDir, id string) string {
	t.Helper()
	dir := filepath.Join(vmsDir, id)
	for _, sub := range []string{rootDir, stateDir} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, rootDir, "user-file.txt"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func mustExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("%s: %v; want it kept", path, err)
	}
}

func mustNotExist(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("%s: %v; want it removed", path, err)
	}
}

// TestDeleteUntrackedRemovesSandboxFiles checks that a runtime that did not
// create a sandbox (a restarted agent) removes its directory, that a second
// Delete and a Delete of an ID without a directory succeed, and that sibling
// sandboxes stay.
func TestDeleteUntrackedRemovesSandboxFiles(t *testing.T) {
	vmsDir := t.TempDir()
	// The earlier agent process created the sandbox with a real helper.
	earlier := newRuntimeWithVMsDir(t, vmsDir)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := earlier.Create(context.Background(), vmruntime.Spec{SandboxID: id, Dir: filepath.Join(vmsDir, id), GuestOS: hostGuestOS, BaseImage: hostBaseImage}); err != nil {
		t.Fatal(err)
	}
	// The helper of an earlier agent process ends with that process; this
	// test stops it with the earlier runtime after the restarted runtime
	// removed the files.
	closeRuntimeOnCleanup(t, earlier)
	userFile := filepath.Join(vmsDir, id, rootDir, "user-file.txt")
	if err := os.WriteFile(userFile, []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := writeSandboxFiles(t, vmsDir, "0192f4b6-7c3a-7def-8abc-000000000002")

	restarted := newRuntimeWithVMsDir(t, vmsDir)
	ctx := context.Background()
	if err := restarted.Delete(ctx, id); err != nil {
		t.Fatalf("Delete of an untracked sandbox: %v", err)
	}
	mustNotExist(t, filepath.Join(vmsDir, id))
	if err := restarted.Delete(ctx, id); err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if err := restarted.Delete(ctx, "0192f4b6-7c3a-7def-8abc-000000000003"); err != nil {
		t.Fatalf("Delete of an ID without a directory: %v", err)
	}
	mustExist(t, filepath.Join(sibling, rootDir, "user-file.txt"))
	mustExist(t, filepath.Join(sibling, stateDir))
}

// TestDeleteUntrackedWithoutVMsDirKeepsFiles checks that a runtime that does
// not know the sandbox directories keeps the earlier no-op for an untracked
// sandbox.
func TestDeleteUntrackedWithoutVMsDirKeepsFiles(t *testing.T) {
	vmsDir := t.TempDir()
	id := "0192f4b6-7c3a-7def-8abc-000000000001"
	dir := writeSandboxFiles(t, vmsDir, id)
	rt := newRuntime(t)
	if err := rt.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	mustExist(t, dir)
}

// TestDeleteUntrackedRejectsPathLikeIDs checks that an ID with more than one
// path element cannot address a directory outside the sandbox directories.
func TestDeleteUntrackedRejectsPathLikeIDs(t *testing.T) {
	base := t.TempDir()
	vmsDir := filepath.Join(base, "vms")
	if err := os.Mkdir(vmsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	rt := newRuntimeWithVMsDir(t, vmsDir)
	for _, id := range []string{"", ".", "..", "../outside", "a/b", `a\b`, outside} {
		if err := rt.Delete(context.Background(), id); err == nil {
			t.Errorf("Delete(%q) succeeded; want an error", id)
		}
	}
	mustExist(t, outside)
	mustExist(t, vmsDir)
}

// TestDeleteUntrackedRemovesRecordedSocketDirectory checks that Delete of an
// untracked sandbox removes the socket directory that the sandbox directory
// records (macOS keeps the socket outside the sandbox directory), and ignores
// a record that does not name a socket directory.
func TestDeleteUntrackedRemovesRecordedSocketDirectory(t *testing.T) {
	vmsDir := t.TempDir()
	socketDir, err := os.MkdirTemp(t.TempDir(), socketDirPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(socketDir, "g.sock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	unrelated := t.TempDir()
	id := "0192f4b6-7c3a-7def-8abc-000000000001"
	dir := writeSandboxFiles(t, vmsDir, id)
	if err := recordSocketDir(dir, socketDir); err != nil {
		t.Fatal(err)
	}
	otherID := "0192f4b6-7c3a-7def-8abc-000000000002"
	otherDir := writeSandboxFiles(t, vmsDir, otherID)
	// A record without the socket directory prefix is not removed.
	if err := recordSocketDir(otherDir, unrelated); err != nil {
		t.Fatal(err)
	}
	rt := newRuntimeWithVMsDir(t, vmsDir)
	for _, sandboxID := range []string{id, otherID} {
		if err := rt.Delete(context.Background(), sandboxID); err != nil {
			t.Fatal(err)
		}
	}
	mustNotExist(t, dir)
	mustNotExist(t, socketDir)
	mustNotExist(t, otherDir)
	mustExist(t, unrelated)
}

func TestDeleteRetainsSocketDirectoryRecordOnRemovalFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("socket directory removal must run without root privileges")
	}
	ctx := context.Background()
	vmsDir := t.TempDir()
	rt := newRuntimeWithVMsDir(t, vmsDir)
	closeRuntimeOnCleanup(t, rt)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(vmsDir, id)
	v, err := rt.Create(ctx, vmruntime.Spec{SandboxID: id, Dir: dir, GuestOS: hostGuestOS, BaseImage: hostBaseImage})
	if err != nil {
		t.Fatal(err)
	}
	h := killHelperAndWait(t, rt, v)
	// Linux uses an abstract socket. Give that sandbox the same on-disk
	// cleanup record that Create writes for the filesystem socket on macOS.
	if h.socketDir == "" {
		h.socketDir, err = os.MkdirTemp(t.TempDir(), socketDirPrefix)
		if err != nil {
			t.Fatal(err)
		}
		if err := recordSocketDir(dir, h.socketDir); err != nil {
			t.Fatal(err)
		}
	}
	socketDir := h.socketDir
	// A nonempty read-only directory prevents RemoveAll from unlinking its
	// entries. An empty directory could still be removed from its parent.
	if err := os.WriteFile(filepath.Join(socketDir, "cleanup-test"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(socketDir, 0o700); err != nil && !os.IsNotExist(err) {
			t.Errorf("restore socket directory permissions: %v", err)
		}
	})
	if err := rt.Delete(ctx, id); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("Delete with a read-only socket directory = %v; want a permission error", err)
	}
	if got, err := rt.lookup(id); err != nil || got != h {
		t.Fatalf("lookup after failed Delete = %p, %v; want the tracked sandbox %p", got, err, h)
	}
	record, err := os.ReadFile(filepath.Join(dir, socketDirFile))
	if err != nil || string(record) != socketDir {
		t.Fatalf("socket directory record after failed Delete = %q, %v; want %q", record, err, socketDir)
	}

	if err := os.Chmod(socketDir, 0o700); err != nil {
		t.Fatal(err)
	}
	restarted := newRuntimeWithVMsDir(t, vmsDir)
	if err := restarted.Delete(ctx, id); err != nil {
		t.Fatalf("Delete after restart: %v", err)
	}
	mustNotExist(t, socketDir)
	mustNotExist(t, dir)
	if err := restarted.Delete(ctx, id); err != nil {
		t.Fatalf("second Delete after restart: %v", err)
	}
}

// TestRecordSocketDirNoopWithoutSocketDirectory checks that a sandbox with an
// abstract socket (Linux) gets no record file.
func TestRecordSocketDirNoopWithoutSocketDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sb")
	if err := recordSocketDir(dir, ""); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, dir)
}

// TestCloseRemovesSandboxWhoseHelperExited checks that Close removes every
// tracked sandbox, including one whose helper exited, so that its files do
// not outlive the agent process.
func TestCloseRemovesSandboxWhoseHelperExited(t *testing.T) {
	vmsDir := t.TempDir()
	rt := newRuntimeWithVMsDir(t, vmsDir)
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	v, err := rt.Create(context.Background(), vmruntime.Spec{SandboxID: id, Dir: filepath.Join(vmsDir, id), GuestOS: hostGuestOS, BaseImage: hostBaseImage})
	if err != nil {
		t.Fatal(err)
	}
	h := killHelperAndWait(t, rt, v)
	if err := rt.Close(); err != nil {
		t.Fatal(err)
	}
	mustNotExist(t, filepath.Join(vmsDir, id))
	if h.socketDir != "" {
		mustNotExist(t, h.socketDir)
	}
	rt.mu.Lock()
	tracked := len(rt.vms)
	rt.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("%d sandboxes tracked after Close; want none", tracked)
	}
}
