// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package processrt

import (
	"context"
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
