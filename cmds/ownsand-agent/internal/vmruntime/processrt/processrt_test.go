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
	"strconv"
	"strings"
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
	dir, err := os.MkdirTemp("", "ownsand-processrt-test-")
	if err != nil {
		panic(err)
	}
	guestBinary = filepath.Join(dir, "ownsand-guest")
	out, err := exec.Command("go", "build", "-o", guestBinary, "github.com/delinoio/mono/cmds/ownsand-guest").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build guest helper: %v\n%s", err, out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
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

func newRuntime(t *testing.T) *Runtime {
	t.Helper()
	rt, err := New(guestBinary, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

func createSandbox(t *testing.T, rt *Runtime) vmruntime.VM {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	// A short directory keeps the unix socket path below the macOS limit.
	dir, err := os.MkdirTemp("", "pr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	v, err := rt.Create(context.Background(), vmruntime.Spec{
		SandboxID: id,
		Dir:       filepath.Join(dir, "sb"),
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
	aHelper := helper.cmd.Process.Pid
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
	helperPID := helper.cmd.Process.Pid

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
	helperPID := helper.cmd.Process.Pid

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

// killHelperAndWait kills the helper of v by its positive process ID, so that
// only the helper dies, and waits until the runtime has reaped it. It returns
// the helper.
func killHelperAndWait(t *testing.T, rt *Runtime, v vmruntime.VM) *vm {
	t.Helper()
	h, err := rt.lookup(v.SandboxID())
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(h.cmd.Process.Pid, syscall.SIGKILL); err != nil {
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
	h.cmd = &exec.Cmd{Process: cmd.Process}
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
	helperPID := h.cmd.Process.Pid

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
	helperPID := h.cmd.Process.Pid

	// An empty PATH makes the `ps` command of the process listing fail.
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", t.TempDir()); err != nil {
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
