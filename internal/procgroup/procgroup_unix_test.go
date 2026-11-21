// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package procgroup

import (
	"os/exec"
	"syscall"
	"testing"
)

// TestSignalStopsTheGroup starts a shell that starts a child, checks that
// both are in the group that Set created, and checks that one Signal to the
// leader stops the group. It does not wait for the child to disappear: a
// killed child can stay a zombie when no init process reaps it.
func TestSignalStopsTheGroup(t *testing.T) {
	cmd := exec.Command("sh", "-c", "sleep 60 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
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
