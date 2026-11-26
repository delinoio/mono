// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// envRunMain makes the test binary run the CLI entry point as a child process.
const envRunMain = "OWNSAND_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(envRunMain) == "1" {
		// The parent passes the acknowledgement pipe as the first ExtraFile.
		ack := os.NewFile(3, "interrupt acknowledgement")
		os.Exit(run(func() {
			_, _ = ack.Write([]byte{1})
		}))
	}
	os.Exit(m.Run())
}

// holdsFile reports whether the process has path open, by reading /proc.
func holdsFile(pid int, path string) (bool, error) {
	dir := filepath.Join("/proc", strconv.Itoa(pid), "fd")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if target, err := os.Readlink(filepath.Join(dir, e.Name())); err == nil && target == path {
			return true, nil
		}
	}
	return false, nil
}

// A command that cannot observe the context (here, a read of a FIFO through
// --secret-file) ignores the first interrupt. The second interrupt must end
// the process: main stops catching the signal once the first one canceled the
// context.
func TestSecondInterruptEndsACommandThatIgnoresTheFirst(t *testing.T) {
	const phaseTimeout = 30 * time.Second

	if _, err := os.ReadDir("/proc/self/fd"); err != nil {
		t.Skipf("/proc is not available: %v", err)
	}
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
	t.Cleanup(func() { _ = hold.Close() })

	ackReader, ackWriter, err := os.Pipe()
	if err != nil {
		t.Fatalf("create the acknowledgement pipe: %v", err)
	}
	t.Cleanup(func() { _ = ackReader.Close() })

	cmd := exec.Command(os.Args[0], "member", "accept", "--secret-file", fifo)
	cmd.ExtraFiles = []*os.File{ackWriter}
	cmd.Env = append(os.Environ(),
		envRunMain+"=1",
		"OWNSAND_API_KEY=osk_testkey",
		"OWNSAND_API_URL=http://127.0.0.1:1",
		"OWNSAND_ORG=00000000-0000-7000-8000-000000000000",
	)
	startErr := cmd.Start()
	// Close the parent's writer so that a child exit before acknowledgement
	// produces EOF on the reader.
	_ = ackWriter.Close()
	if startErr != nil {
		t.Fatalf("start the command: %v", startErr)
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
	deadline := time.Now().Add(phaseTimeout)
	for {
		held, err := holdsFile(cmd.Process.Pid, fifo)
		if err != nil {
			t.Fatalf("inspect the command before FIFO readiness: %v", err)
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

	if err := ackReader.SetReadDeadline(time.Now().Add(phaseTimeout)); err != nil {
		t.Fatalf("set the acknowledgement deadline: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send the first SIGINT: %v", err)
	}
	var ack [1]byte
	if _, err := io.ReadFull(ackReader, ack[:]); err != nil {
		if errors.Is(err, io.EOF) {
			t.Fatal("the command exited before acknowledging the first SIGINT")
		}
		t.Fatalf("the command did not acknowledge the first SIGINT: %v", err)
	}
	held, err := holdsFile(cmd.Process.Pid, fifo)
	if err != nil {
		t.Fatalf("inspect the command after the first SIGINT: %v", err)
	}
	if !held {
		t.Fatal("the command did not keep the FIFO open after the first SIGINT")
	}

	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("send the second SIGINT: %v", err)
	}
	select {
	case <-ended:
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			t.Fatalf("the command ended without a signal: %v", waitErr)
		}
		status, ok := exitErr.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGINT {
			t.Fatalf("the command ended with %v, want termination by SIGINT", exitErr)
		}
	case <-time.After(phaseTimeout):
		t.Fatal("the command did not end after the second SIGINT")
	}
}
