// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package sandruntest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

const execGroupSetup = `sleep 30 & printf '%s %s\n' "$$" "$!" > "$PID_FILE"; printf ready`

func TestExecDeadlineDuringOutputDrain(t *testing.T) {
	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	workspace := t.TempDir()
	client, ds := createExecTestSession(t, ctx, s, workspace)
	pidFile := execGroupPIDFile(t, workspace)
	const timeout = 300 * time.Millisecond
	startedAt := time.Now()
	stream, err := client.ExecDevSession(ctx, listTestRequest(&sandrunv1.ExecDevSessionRequest{
		OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: streamTestExecID,
		Command: []string{"/bin/sh", "-c", execGroupSetup + "; exit 0"},
		Env:     map[string]string{"PID_FILE": pidFile},
		Timeout: durationpb.New(timeout),
	}))
	events := receiveExecEvents(t, stream, err)
	if elapsed := time.Since(startedAt); elapsed < timeout || elapsed >= 3*time.Second {
		t.Errorf("exec finished in %s, want the pipe drain to span the deadline and remain bounded", elapsed)
	}
	final := checkExecGroupEvents(t, events)
	if !hasExplicitZeroExitCode(final) || final.GetExited().GetTimedOut() {
		t.Errorf("terminal event = %v, want exit code 0 without timeout", final)
	}

	leader, child := readExecGroupPIDs(t, pidFile)
	if execTestProcessRunning(leader) || !execTestProcessRunning(child) {
		t.Errorf("expected exited leader %d and surviving descendant %d", leader, child)
	}

	attach, err := client.AttachDevSessionExec(ctx, listTestRequest(&sandrunv1.AttachDevSessionExecRequest{
		OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: streamTestExecID,
	}))
	attached := checkExecGroupEvents(t, receiveExecEvents(t, attach, err))
	if !proto.Equal(final, attached) {
		t.Errorf("attach reported %v, want %v", attached, final)
	}
	s.Close()
	if !execTestProcessRunning(child) {
		t.Errorf("Close signaled the descendant of a completed exec")
	}
}

func TestExecGroupCancellation(t *testing.T) {
	for _, stop := range []string{"deadline", "server close"} {
		t.Run(stop, func(t *testing.T) {
			s := New()
			t.Cleanup(s.Close)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			workspace := t.TempDir()
			client, ds := createExecTestSession(t, ctx, s, workspace)
			pidFile := execGroupPIDFile(t, workspace)
			req := &sandrunv1.ExecDevSessionRequest{
				OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: streamTestExecID,
				Command: []string{"/bin/sh", "-c", execGroupSetup + "; wait"},
				Env:     map[string]string{"PID_FILE": pidFile},
			}
			deadline := stop == "deadline"
			if deadline {
				req.Timeout = durationpb.New(300 * time.Millisecond)
			}
			stream, err := client.ExecDevSession(ctx, listTestRequest(req))
			var final *sandrunv1.DevSessionExecEvent
			if deadline {
				final = checkExecGroupEvents(t, receiveExecEvents(t, stream, err))
			} else {
				if err != nil {
					t.Fatal(err)
				}
				defer stream.Close()
				leader, child := readExecGroupPIDs(t, pidFile)
				if !execTestProcessRunning(leader) || !execTestProcessRunning(child) {
					t.Fatal("expected a running process group before Close")
				}
				s.Close()
				// Close also closes the HTTP server. Inspect the retained result.
				final = s.guest.execs[streamTestExecID].final
			}
			exited := final.GetExited()
			if exited.GetSignal() != int32(syscall.SIGKILL) || exited.GetTimedOut() != deadline {
				t.Errorf("terminal event = %v, want SIGKILL and timed_out=%t", final, deadline)
			}
			leader, child := readExecGroupPIDs(t, pidFile)
			waitForExecTestProcessExit(t, leader)
			waitForExecTestProcessExit(t, child)
		})
	}
}

func TestCloseDuringExecOutputDrain(t *testing.T) {
	testCloseDuringExecOutputDrain(t, false)
}

func TestCloseRacesExecOutputDrainCompletion(t *testing.T) {
	testCloseDuringExecOutputDrain(t, true)
}

func testCloseDuringExecOutputDrain(t *testing.T, raceCompletion bool) {
	t.Helper()
	s := New()
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	workspace := t.TempDir()
	client, ds := createExecTestSession(t, ctx, s, workspace)
	pidFile := execGroupPIDFile(t, workspace)
	stream, err := client.ExecDevSession(ctx, listTestRequest(&sandrunv1.ExecDevSessionRequest{
		OrganizationId: ds.GetOrganizationId(), DevSessionId: ds.GetDevSessionId(), ExecId: streamTestExecID,
		Command: []string{"/bin/sh", "-c", "trap 'exit 17' USR1; " + execGroupSetup + "; wait"},
		Env:     map[string]string{"PID_FILE": pidFile},
	}))
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	leader, child := readExecGroupPIDs(t, pidFile)
	if !execTestProcessRunning(child) {
		t.Fatalf("descendant %d was not running before the direct child exited", child)
	}
	groupID, err := syscall.Getpgid(child)
	if err != nil {
		t.Fatal(err)
	}
	s.guest.mu.Lock()
	p := s.guest.execs[streamTestExecID]
	s.guest.mu.Unlock()
	// Start the one-second drain window only after the process checks and setup.
	if err := syscall.Kill(leader, syscall.SIGUSR1); err != nil {
		t.Fatal(err)
	}
	waitForExecTestProcessReaped(t, leader)
	if groupID == leader || syscall.Kill(groupID, 0) != nil {
		t.Errorf("process group %d has no fixture anchor after command %d exited", groupID, leader)
	}
	p.mu.Lock()
	final := p.final
	p.mu.Unlock()
	if final != nil {
		t.Fatal("exec finished before Close could interrupt output drain")
	}

	if raceCompletion {
		// Closing the descendant's pipe lets Wait finish independently of Close.
		// Either operation can win ownership of group cleanup.
		stopped := make(chan error, 1)
		go func() { stopped <- syscall.Kill(child, syscall.SIGTERM) }()
		s.Close()
		if err := <-stopped; err != nil && !errors.Is(err, syscall.ESRCH) {
			t.Fatal(err)
		}
	} else {
		s.Close()
	}
	if execTestProcessRunning(child) {
		t.Fatalf("descendant %d was still running when Close returned", child)
	}
	if !errors.Is(syscall.Kill(groupID, 0), syscall.ESRCH) {
		t.Errorf("process group anchor %d was not reaped", groupID)
	}
	exited := p.final.GetExited()
	if exited.GetExitCode() != 17 || exited.GetTimedOut() || string(p.out[0].data) != "ready" {
		t.Errorf("stdout = %q, terminal event = %v, want ready and exit 17 without timeout", p.out[0].data, p.final)
	}
}

func TestExecDeadlineSkipsExitedGroupLeader(t *testing.T) {
	pidFile := execGroupPIDFile(t, t.TempDir())
	cmd := newExecCommand(t.Context(), []string{"/bin/sh", "-c", execGroupSetup + "; exit 0"}, map[string]string{"PID_FILE": pidFile})
	group := new(execGroup)
	t.Cleanup(group.release)
	if err := group.start(t.Context(), cmd); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	leader, child := readExecGroupPIDs(t, pidFile)
	waitForExecTestProcessExit(t, leader)

	checkDeadline := func(phase string) {
		t.Helper()
		if err := group.cancel(true); !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("deadline with %s leader returned %v, want os.ErrProcessDone", phase, err)
		}
		if !execTestProcessRunning(child) {
			t.Errorf("deadline with %s leader killed descendant %d", phase, child)
		}
	}
	checkDeadline("unreaped")
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	checkDeadline("reaped")
}

func checkExecGroupEvents(t *testing.T, events []*sandrunv1.DevSessionExecEvent) *sandrunv1.DevSessionExecEvent {
	t.Helper()
	requireStarted(t, events, 0)
	var stdout strings.Builder
	var exits int
	for _, event := range events {
		stdout.Write(event.GetStdout().GetData())
		if event.GetExited() != nil {
			exits++
		}
	}
	final := events[len(events)-1]
	if stdout.String() != "ready" || exits != 1 || final.GetExited() == nil {
		t.Fatalf("events = %v, want retained stdout and one final exit", events)
	}
	return final
}

func execGroupPIDFile(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "pids")
	t.Cleanup(func() {
		_, child, err := loadExecGroupPIDs(path)
		if err == nil {
			_ = syscall.Kill(child, syscall.SIGKILL)
		}
	})
	return path
}

func readExecGroupPIDs(t *testing.T, path string) (int, int) {
	t.Helper()
	var leader, child int
	var err error
	for range 100 {
		leader, child, err = loadExecGroupPIDs(path)
		if err == nil {
			return leader, child
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("could not read process IDs: %v", err)
	return 0, 0
}

func loadExecGroupPIDs(path string) (int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, err
	}
	var leader, child int
	if _, err := fmt.Sscan(string(data), &leader, &child); err != nil {
		return 0, 0, err
	}
	if leader <= 0 || child <= 0 {
		return 0, 0, fmt.Errorf("invalid process IDs %d and %d", leader, child)
	}
	return leader, child, nil
}

func execTestProcessRunning(pid int) bool {
	out, err := exec.Command("/bin/ps", "-o", "state=", "-p", strconv.Itoa(pid)).Output()
	state := strings.TrimSpace(string(out))
	return err == nil && state != "" && !strings.HasPrefix(state, "Z")
}

func waitForExecTestProcessExit(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if !execTestProcessRunning(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d is still running", pid)
}

func waitForExecTestProcessReaped(t *testing.T, pid int) {
	t.Helper()
	for range 100 {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("process %d was not reaped", pid)
}
