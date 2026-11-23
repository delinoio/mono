// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// envRunMain makes the test binary run main instead of the tests, so that a
// test can start the real entry point as a child process.
const envRunMain = "OWNSAND_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(envRunMain) == "1" {
		main()
		return
	}
	os.Exit(m.Run())
}

// holdsFile reports whether the process has path open, by reading /proc.
func holdsFile(pid int, path string) (held, supported bool) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, false
	}
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && target == path {
			return true, true
		}
	}
	return false, true
}

// A command that cannot observe the context (here, a read of a FIFO through
// --secret-file) ignores the first interrupt. The second interrupt must end
// the process: main stops catching the signal once the first one canceled the
// context.
func TestSecondInterruptEndsACommandThatIgnoresTheFirst(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "secret.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo is not available: %v", err)
	}
	// Holding the FIFO open for reading and writing lets the command open it
	// at once; its read then blocks because no data arrives.
	hold, err := os.OpenFile(fifo, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open the FIFO: %v", err)
	}
	defer hold.Close()

	cmd := exec.Command(os.Args[0], "member", "accept", "--secret-file", fifo)
	cmd.Env = append(os.Environ(),
		envRunMain+"=1",
		"OWNSAND_API_KEY=osk_testkey",
		"OWNSAND_API_URL=http://127.0.0.1:1",
		"OWNSAND_ORG=00000000-0000-7000-8000-000000000000",
	)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start the command: %v", err)
	}
	var waitErr error
	ended := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(ended) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-ended
	})

	// Wait until the command reads the FIFO, which is after main installed
	// the interrupt handler.
	deadline := time.Now().Add(30 * time.Second)
	for {
		held, supported := holdsFile(cmd.Process.Pid, fifo)
		if !supported {
			t.Skip("/proc is not available")
		}
		if held {
			break
		}
		select {
		case <-ended:
			t.Fatalf("the command ended before it opened the FIFO: %v", waitErr)
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not open the FIFO")
		}
	}

	// Send interrupts until the process ends. The first one cancels the
	// context and is otherwise ignored by this command; a later one takes the
	// default action. Sending more than one is safe: the process is gone
	// after the one that arrives after main stopped catching the signal.
	for {
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case <-ended:
			var ee *exec.ExitError
			if !errors.As(waitErr, &ee) {
				t.Fatalf("the command ended without a signal: %v", waitErr)
			}
			ws, ok := ee.Sys().(syscall.WaitStatus)
			if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGINT {
				t.Fatalf("the command ended with %v, want termination by SIGINT", ee)
			}
			return
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline.Add(30 * time.Second)) {
			t.Fatal("the process did not end after repeated interrupts")
		}
	}
}
