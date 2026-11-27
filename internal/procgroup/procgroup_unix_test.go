// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package procgroup

import (
	"io"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// childExitDeadline bounds how long the group signal test waits for the child
// to die after the signal. The group kill normally takes effect immediately;
// the bound only decides how long a failing run takes.
const childExitDeadline = 5 * time.Second

// TestSignalStopsTheGroup starts a shell that starts a child, checks that
// both are in the group that Set created, and checks that one Signal to the
// leader stops the group. The child inherits the write end of the leader's
// stdout pipe, so reading the pipe to EOF succeeds only after both the shell
// and the child have died. A killed child that no init process reaps stays a
// zombie, but a zombie holds no file descriptors, so the check does not depend
// on reaping.
func TestSignalStopsTheGroup(t *testing.T) {
	// The child's sleep outlasts the signal check and bounds a failed run.
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	Set(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var childPID int
	buf := make([]byte, 32)
	n, err := out.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range buf[:n] {
		if c >= '0' && c <= '9' {
			childPID = childPID*10 + int(c-'0')
		}
	}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err != nil || pgid != cmd.Process.Pid {
		t.Fatalf("leader process group = %d, %v; want %d", pgid, err, cmd.Process.Pid)
	}
	if pgid, err := syscall.Getpgid(childPID); err != nil || pgid != cmd.Process.Pid {
		t.Fatalf("child process group = %d, %v; want %d", pgid, err, cmd.Process.Pid)
	}
	if err := Signal(cmd.Process, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	drained := make(chan error, 1)
	go func() {
		_, err := io.Copy(io.Discard, out)
		drained <- err
	}()
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("read leader stdout: %v", err)
		}
	case <-time.After(childExitDeadline):
		t.Fatalf("child %d survived the group signal", childPID)
	}
	err = cmd.Wait()
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("leader did not stop with SIGKILL: %v", err)
	}
}

func TestSignalNilProcess(t *testing.T) {
	if err := Signal(nil, syscall.SIGTERM); err != nil {
		t.Fatalf("Signal(nil) = %v", err)
	}
}
