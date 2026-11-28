// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// The FIFO opened by os.ReadFile can use Go's poller. Inherited stdin is
// blocking, so a thread must be in read(0, ...) while it waits for data.
func waitingForCredential(pid int, fifo string) bool {
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	if fifo != "" {
		fds, _ := filepath.Glob(filepath.Join(proc, "fd", "*"))
		for _, fd := range fds {
			if target, err := os.Readlink(fd); err == nil && target == fifo {
				return true
			}
		}
		return false
	}
	threads, _ := filepath.Glob(filepath.Join(proc, "task", "*", "syscall"))
	read := strconv.Itoa(syscall.SYS_READ)
	for _, thread := range threads {
		inCall, err := os.ReadFile(thread)
		if err != nil {
			continue
		}
		fields := strings.Fields(string(inCall))
		if len(fields) >= 2 && fields[0] == read && fields[1] == "0x0" {
			return true
		}
	}
	return false
}

func TestSecondInterruptEndsBlockedCredentialRead(t *testing.T) {
	for _, input := range []string{"fifo", "stdin"} {
		t.Run(input, func(t *testing.T) {
			testBlockedCredentialInterrupt(t, input, acknowledgeInterrupt, 5*time.Second)
		})
	}
}

func TestBlockedCredentialInterruptRejectsThirdSignal(t *testing.T) {
	testBlockedCredentialInterrupt(t, "stdin", consumeSecondInterrupt, 250*time.Millisecond)
}

func testBlockedCredentialInterrupt(t *testing.T, input, mode string, exitTimeout time.Duration) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	t.Cleanup(srv.Close)
	args := []string{"--org", "sandrun-org", "ownsand", "link", "--ownsand-org", "ownsand-org"}
	var fifo string
	var stdin *os.File
	switch input {
	case "fifo":
		// Match the resolved path that /proc reports for the open file.
		dir, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		fifo = filepath.Join(dir, "key.fifo")
		if err := syscall.Mkfifo(fifo, 0o600); err != nil {
			t.Fatal(err)
		}
		// Keep a writer open without sending data or EOF.
		hold, err := os.OpenFile(fifo, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = hold.Close() })
		args = append(args, "--api-key-file", fifo)
	case "stdin":
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = w.Close()
			_ = r.Close()
		})
		stdin = r
	}
	cmd := sandRunCommand(t, srv.URL, args...)
	if stdin != nil {
		cmd.Stdin = stdin
	}
	ack, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer ack.Close()
	defer writer.Close()
	cmd.ExtraFiles = []*os.File{writer}
	cmd.Env = append(cmd.Env, runSandRunMain+"="+mode)
	if mode == consumeSecondInterrupt {
		// startSandRun registers its cleanup after this assertion, so it reaps
		// the child first. The credential writer stays open until then.
		t.Cleanup(func() {
			if cmd.ProcessState == nil || cmd.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Errorf("cleanup must kill and reap the blocked child, got %v", cmd.ProcessState)
			}
		})
	}
	ended := startSandRun(t, cmd)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(10 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for !waitingForCredential(cmd.Process.Pid, fifo) {
		select {
		case <-ended:
			t.Fatalf("SandRun exited before opening credential input: %v", cmd.ProcessState)
		case <-deadline:
			t.Fatal("SandRun did not reach the credential read")
		case <-tick.C:
		}
	}

	sendInterrupt := func(ordinal string) {
		t.Helper()
		select {
		case <-ended:
			t.Fatalf("SandRun exited before the %s interrupt: %v", ordinal, cmd.ProcessState)
		default:
		}
		if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
			t.Fatalf("send the %s SIGINT: %v", ordinal, err)
		}
	}
	readAcknowledgment := func(ordinal string) {
		t.Helper()
		if err := ack.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			t.Fatal(err)
		}
		var marker [1]byte
		if _, err := io.ReadFull(ack, marker[:]); err != nil {
			t.Fatalf("wait for acknowledgment of the %s SIGINT: %v", ordinal, err)
		}
	}
	sendInterrupt("first")
	readAcknowledgment("first")
	sendInterrupt("second")
	select {
	case <-ended:
		if mode == consumeSecondInterrupt {
			t.Fatalf("control child exited after two interrupts: %v", cmd.ProcessState)
		}
		if status := cmd.ProcessState.Sys().(syscall.WaitStatus); status.Signal() != syscall.SIGINT {
			t.Fatalf("SandRun ended with %v, want SIGINT", cmd.ProcessState)
		}
	case <-time.After(exitTimeout):
		if mode != consumeSecondInterrupt {
			t.Fatal("SandRun did not exit after the second SIGINT")
		}
		readAcknowledgment("second")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("SandRun sent %d requests during credential input", got)
	}
}
