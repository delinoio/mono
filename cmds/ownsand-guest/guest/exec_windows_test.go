// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestTerminateEndsTheProcessTree starts cmd.exe with a ping child and
// checks that terminate ends both through the job object.
func TestTerminateEndsTheProcessTree(t *testing.T) {
	cmd := exec.Command("cmd.exe", "/c", "ping -n 60 127.0.0.1 >nul")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	var tree []uint32
	deadline := time.Now().Add(10 * time.Second)
	for {
		var err error
		tree, err = processTree(uint32(cmd.Process.Pid))
		if err != nil {
			t.Fatal(err)
		}
		if len(tree) >= 2 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(tree) < 2 {
		t.Fatalf("process tree %v has no child", tree)
	}
	var handles []windows.Handle
	for _, pid := range tree {
		h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
		if err != nil {
			t.Fatalf("open process %d: %v", pid, err)
		}
		defer windows.CloseHandle(h)
		handles = append(handles, h)
	}
	terminate(cmd.Process)
	for i, h := range handles {
		if ev, err := windows.WaitForSingleObject(h, 10000); err != nil || ev != windows.WAIT_OBJECT_0 {
			t.Fatalf("process %d still runs after terminate (%#x, %v)", tree[i], ev, err)
		}
	}
	_ = cmd.Wait()
}

// helperModeEnv selects what TestHelperProcess does when a test starts the
// test binary as a child process (the usual Go pattern for a portable
// fixture, so that the tests need neither cmd.exe nor ping timing).
const (
	helperModeEnv    = "OWNSAND_GUEST_TEST_HELPER_MODE"
	helperPidFileEnv = "OWNSAND_GUEST_TEST_HELPER_PID_FILE"
	// helperLifetime bounds a helper that nothing killed, so that a failed
	// test cannot leave a process behind for long.
	helperLifetime = 60 * time.Second
)

// TestHelperProcess is not a test. The tests below start the test binary with
// -test.run=^TestHelperProcess$ and a helperModeEnv value:
//
//   - "exit" exits at once with code 0;
//   - "sleep" writes its process ID to helperPidFileEnv when that is set and
//     sleeps for helperLifetime;
//   - "spawn" starts a "sleep" child that inherits stdout and stderr, writes
//     the child's process ID to helperPidFileEnv and exits with code 0, so
//     the direct child of the exec ends while a descendant holds the output
//     pipes.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	pidFile := os.Getenv(helperPidFileEnv)
	switch mode {
	case "exit":
		os.Exit(0)
	case "sleep":
		if pidFile != "" {
			_ = os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(helperLifetime)
		os.Exit(0)
	case "spawn":
		child := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
		child.Env = append(os.Environ(), helperModeEnv+"=sleep", helperPidFileEnv+"=")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		os.Exit(0)
	}
	os.Exit(4)
}

// helperCommand returns the exec request command that runs the test binary
// as a helper process.
func helperCommand() []string {
	return []string{os.Args[0], "-test.run=^TestHelperProcess$"}
}

// helperExecReq returns an exec request that runs the test binary as a helper
// in the given mode. The POSIX-only execReq lives in exec_test.go.
func helperExecReq(t *testing.T, mode, pidFile string, timeout time.Duration) *ownsandv1.ExecRequest {
	t.Helper()
	return &ownsandv1.ExecRequest{
		SandboxId: testSandbox,
		ExecId:    newID(t),
		Command:   helperCommand(),
		Env:       map[string]string{helperModeEnv: mode, helperPidFileEnv: pidFile},
		Timeout:   durationpb.New(timeout),
	}
}

// waitForPidFile waits for a helper to write its pid file.
func waitForPidFile(t *testing.T, path string) uint32 {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil && pid > 0 {
				return uint32(pid)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("pid file %s was not written", path)
	return 0
}

// killPidFileOnCleanup ends the process whose ID the helper wrote to pidFile
// at the end of the test, even when the test failed before it checked the
// process. A missing pid file or a process that already ended is not an
// error.
func killPidFileOnCleanup(t *testing.T, pidFile string) {
	t.Cleanup(func() {
		b, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		pid, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32)
		if err != nil {
			return
		}
		h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
		if err != nil {
			return
		}
		defer windows.CloseHandle(h)
		_ = windows.TerminateProcess(h, 1)
	})
}

// processEnded reports whether pid is gone, waiting up to wait for it.
func processEnded(t *testing.T, pid uint32, wait time.Duration) bool {
	t.Helper()
	h, err := windows.OpenProcess(windows.SYNCHRONIZE, false, pid)
	if err != nil {
		return true
	}
	defer windows.CloseHandle(h)
	ev, err := windows.WaitForSingleObject(h, uint32(wait.Milliseconds()))
	if err != nil {
		t.Fatalf("wait for process %d: %v", pid, err)
	}
	return ev == windows.WAIT_OBJECT_0
}

// TestProcessExited pins the os.Process states that processExited reads, so
// that a Go release that changes them fails here instead of timing out
// finished commands again.
func TestProcessExited(t *testing.T) {
	start := func(t *testing.T, mode string) *exec.Cmd {
		t.Helper()
		cmd := exec.Command(helperCommand()[0], helperCommand()[1:]...)
		cmd.Env = append(os.Environ(), helperModeEnv+"="+mode, helperPidFileEnv+"=")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = cmd.Process.Kill() })
		return cmd
	}

	t.Run("running", func(t *testing.T) {
		cmd := start(t, "sleep")
		if processExited(cmd.Process) {
			t.Fatal("a running process counts as exited")
		}
	})
	t.Run("killed and waited", func(t *testing.T) {
		cmd := start(t, "sleep")
		if err := cmd.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		_ = cmd.Wait()
		if !processExited(cmd.Process) {
			t.Fatal("a killed process that Wait reaped does not count as exited")
		}
	})
	t.Run("exited and waited", func(t *testing.T) {
		cmd := start(t, "exit")
		if err := cmd.Wait(); err != nil {
			t.Fatal(err)
		}
		if !processExited(cmd.Process) {
			t.Fatal("a process that exited and that Wait reaped does not count as exited")
		}
	})
}

// TestExecNoTimeoutWhenDescendantHoldsPipeAfterExit runs a command whose
// direct child exits at once and leaves a descendant that holds stdout, so
// cmd.Wait drains the pipe for pipeWaitDelay. The timeout fires during that
// drain: the command already ended, so it must not be timed out and the
// descendant must not be terminated. The timeout is longer than the time the
// direct child needs to start a process and shorter than the drain after it,
// so the margins on both sides are about a second.
func TestExecNoTimeoutWhenDescendantHoldsPipeAfterExit(t *testing.T) {
	h := newHarness(t, nil)
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	req := helperExecReq(t, "spawn", pidFile, 1500*time.Millisecond)
	// Registered before the run so that the descendant dies on a failure too.
	killPidFileOnCleanup(t, pidFile)
	start := time.Now()
	evs := h.runExec(req)
	elapsed := time.Since(start)
	pid := waitForPidFile(t, pidFile)
	if err := requireExplicitSuccess(evs[len(evs)-1]); err != nil {
		t.Fatalf("exec ended after %v: %v", elapsed, err)
	}
	if elapsed < req.GetTimeout().AsDuration() {
		t.Fatalf("the command ended after %v, before its timeout, so the test did not cover the drain", elapsed)
	}
	if processEnded(t, pid, 500*time.Millisecond) {
		t.Fatal("a command that exited before its timeout had its descendant terminated")
	}
}

// TestExecTimeoutKillsLiveChild checks that a direct child that outlives its
// timeout is still terminated and reported as timed out.
func TestExecTimeoutKillsLiveChild(t *testing.T) {
	h := newHarness(t, nil)
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	req := helperExecReq(t, "sleep", pidFile, 1500*time.Millisecond)
	killPidFileOnCleanup(t, pidFile)
	start := time.Now()
	evs := h.runExec(req)
	ex := evs[len(evs)-1].GetExited()
	if !ex.GetTimedOut() {
		t.Fatalf("exited %v after %v", ex, time.Since(start))
	}
	// The helper sleeps for helperLifetime, so an exit well before that came
	// from the termination.
	if time.Since(start) > 20*time.Second {
		t.Fatalf("the timeout took %v", time.Since(start))
	}
	// A helper that the timeout ended before it wrote its pid file leaves
	// nothing to check; the timed-out result above already covers it.
	if b, err := os.ReadFile(pidFile); err == nil {
		if pid, err := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 32); err == nil && !processEnded(t, uint32(pid), 10*time.Second) {
			t.Fatal("the child still runs after its timeout")
		}
	}
}
