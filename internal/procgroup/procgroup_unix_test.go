// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package procgroup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const (
	fixtureIOTimeout = 5 * time.Second
	pidRecordSize    = 32
	reportFixturePID = `printf '%s\n' "$!"`
)

type signalFixture struct {
	cmd       *exec.Cmd
	out       *os.File
	stdout    *bufio.Reader
	stopChild *os.File
	wait      func() error
	stdoutEOF bool
}

func startSignalFixture(t *testing.T, pidReport string, pgid int) *signalFixture {
	t.Helper()
	out, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = out.Close() })
	defer writer.Close()

	control, stopChild, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stopChild.Close() })
	defer control.Close()

	// The child keeps stdout open while it reads the private control pipe.
	// Closing the write end also stops a child outside the shell's group.
	cmd := exec.Command("sh", "-c", "(IFS= read -r _ <&3) &\n"+pidReport+"\nwait")
	cmd.ExtraFiles = []*os.File{control}
	cmd.Stdout = writer
	Set(cmd)
	if pgid != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: pgid}
	}
	f := &signalFixture{
		cmd:       cmd,
		out:       out,
		stdout:    bufio.NewReaderSize(out, pidRecordSize),
		stopChild: stopChild,
		wait:      sync.OnceValue(cmd.Wait),
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !f.stdoutEOF {
			f.stop(t)
		}
		f.wait()
	})
	return f
}

func (f *signalFixture) stop(t *testing.T) {
	t.Helper()
	defer f.out.Close()
	_ = f.stopChild.Close()

	// Verify ownership before Wait releases the shell's PID.
	// Use the owned Process if the shell does not lead a dedicated group.
	leader := f.cmd.Process
	stoppedGroup := false
	if pgid, err := syscall.Getpgid(leader.Pid); err == nil && pgid == leader.Pid {
		stoppedGroup = syscall.Kill(-pgid, syscall.SIGKILL) == nil
	}
	if !stoppedGroup {
		if err := leader.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Errorf("stop fixture shell: %v", err)
		}
	}
	if err := f.drain(fixtureIOTimeout); err != nil {
		t.Errorf("drain fixture stdout during teardown: %v", err)
	}
}

func (f *signalFixture) readPID(timeout time.Duration) (int, error) {
	if err := f.out.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return 0, err
	}
	return readFixturePID(f.stdout)
}

func readFixturePID(stdout *bufio.Reader) (int, error) {
	record, err := stdout.ReadSlice('\n')
	if err != nil {
		return 0, fmt.Errorf("read child PID record: %w", err)
	}
	digits := record[:len(record)-1]
	for _, digit := range digits {
		if digit < '0' || digit > '9' {
			return 0, fmt.Errorf("invalid child PID %q", digits)
		}
	}
	// Unix pid_t is a signed 32-bit integer.
	pid, err := strconv.ParseUint(string(digits), 10, 31)
	if err != nil || pid == 0 {
		return 0, fmt.Errorf("invalid child PID %q", digits)
	}
	return int(pid), nil
}

func (f *signalFixture) checkGroup(childPID int) error {
	leaderPID := f.cmd.Process.Pid
	if pgid, err := syscall.Getpgid(leaderPID); err != nil || pgid != leaderPID {
		return fmt.Errorf("leader process group = %d, %v; want %d", pgid, err, leaderPID)
	}
	if pgid, err := syscall.Getpgid(childPID); err != nil || pgid != leaderPID {
		return fmt.Errorf("child process group = %d, %v; want %d", pgid, err, leaderPID)
	}
	return nil
}

func (f *signalFixture) drain(timeout time.Duration) error {
	if err := f.out.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	_, err := io.Copy(io.Discard, f.stdout)
	f.stdoutEOF = err == nil
	return err
}

// TestSignalStopsTheGroup starts a shell that starts a child, checks that
// both are in the group that Set created, and checks that one Signal to the
// leader stops the group. The child inherits the write end of the leader's
// stdout pipe, so reading the pipe to EOF succeeds only after both the shell
// and the child have died. A killed child that no init process reaps stays a
// zombie, but a zombie holds no file descriptors, so the check does not depend
// on reaping.
func TestSignalStopsTheGroup(t *testing.T) {
	f := startSignalFixture(t, reportFixturePID, 0)
	childPID := fixturePID(t, f)
	if err := f.checkGroup(childPID); err != nil {
		t.Fatal(err)
	}
	if err := Signal(f.cmd.Process, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if err := f.drain(fixtureIOTimeout); err != nil {
		t.Fatalf("wait for shell and child %d to close stdout: %v", childPID, err)
	}
	err := f.wait()
	state := f.cmd.ProcessState
	if state == nil {
		t.Fatalf("missing shell exit status: %v", err)
	}
	if ws, ok := state.Sys().(syscall.WaitStatus); !ok || !ws.Signaled() || ws.Signal() != syscall.SIGKILL {
		t.Fatalf("leader did not stop with SIGKILL: %v", err)
	}
}

func TestReadFixturePID(t *testing.T) {
	stdout := bufio.NewReaderSize(io.MultiReader(strings.NewReader("12"), strings.NewReader("345\nrest")), pidRecordSize)
	if pid, err := readFixturePID(stdout); err != nil || pid != 12345 {
		t.Fatalf("split PID = %d, %v; want 12345", pid, err)
	}
	if rest, err := io.ReadAll(stdout); err != nil || string(rest) != "rest" {
		t.Fatalf("buffer after PID = %q, %v; want rest", rest, err)
	}

	for name, record := range map[string]string{
		"missing":      "",
		"incomplete":   "123",
		"blank":        "\n",
		"whitespace":   " 12\n",
		"text":         "12x3\n",
		"plus sign":    "+12\n",
		"negative":     "-1\n",
		"zero":         "0\n",
		"PID range":    "2147483648\n",
		"overflow":     "999999999999999999999999\n",
		"record limit": strings.Repeat("1", pidRecordSize) + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			stdout := bufio.NewReaderSize(strings.NewReader(record), pidRecordSize)
			if pid, err := readFixturePID(stdout); err == nil || pid != 0 {
				t.Fatalf("PID for %q = %d, %v; want 0 and an error", record, pid, err)
			}
		})
	}
}

func TestSignalFixtureTeardown(t *testing.T) {
	const faultTimeout = 50 * time.Millisecond
	for _, tc := range []struct {
		name           string
		pidReport      string
		joinOtherGroup bool
		exercise       func(*testing.T, *signalFixture)
	}{
		{"partial PID", "printf 12", false, func(t *testing.T, f *signalFixture) {
			expectPIDTimeout(t, f, faultTimeout)
		}},
		{"missing PID", ":", false, func(t *testing.T, f *signalFixture) {
			expectPIDTimeout(t, f, faultTimeout)
		}},
		{"group mismatch", reportFixturePID, true, func(t *testing.T, f *signalFixture) {
			pid := fixturePID(t, f)
			if err := f.checkGroup(pid); err == nil {
				t.Fatal("accepted a shell that does not lead its group")
			}
		}},
		{"drain timeout", reportFixturePID, false, func(t *testing.T, f *signalFixture) {
			pid := fixturePID(t, f)
			if err := f.checkGroup(pid); err != nil {
				t.Fatal(err)
			}
			// Simulate a broken group signal that stops only the shell.
			if err := f.cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := f.drain(faultTimeout); !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("drain after killing only the shell = %v; want a timeout", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pgid := 0
			if tc.joinOtherGroup {
				// Keep the mismatch in a private group so it never involves the test runner.
				other := startSignalFixture(t, reportFixturePID, 0)
				pgid = other.cmd.Process.Pid
				t.Cleanup(func() {
					if err := other.drain(faultTimeout); !errors.Is(err, os.ErrDeadlineExceeded) {
						t.Errorf("stdout in the other fixture closed during fallback: %v", err)
					}
					_ = other.stopChild.Close()
					if err := other.drain(fixtureIOTimeout); err != nil {
						t.Fatalf("drain other fixture after releasing its child: %v", err)
					}
					if err := other.wait(); err != nil {
						t.Errorf("other fixture did not exit normally: %v", err)
					}
				})
			}

			var f *signalFixture
			// Register this first so it runs after fixture teardown.
			t.Cleanup(func() {
				if f == nil {
					return
				}
				if !f.stdoutEOF {
					t.Error("fixture processes still held stdout open during teardown")
				}
				if f.cmd.ProcessState == nil {
					t.Error("fixture shell was not reaped")
				}
				if _, err := f.out.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
					t.Errorf("read after teardown = %v; want a closed pipe", err)
				}
			})
			f = startSignalFixture(t, tc.pidReport, pgid)
			tc.exercise(t, f)
		})
	}
}

func expectPIDTimeout(t *testing.T, f *signalFixture, timeout time.Duration) {
	t.Helper()
	if pid, err := f.readPID(timeout); pid != 0 || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("PID read = %d, %v; want 0 and a timeout", pid, err)
	}
}

func fixturePID(t *testing.T, f *signalFixture) int {
	t.Helper()
	pid, err := f.readPID(fixtureIOTimeout)
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func TestSignalNilProcess(t *testing.T) {
	if err := Signal(nil, syscall.SIGTERM); err != nil {
		t.Fatalf("Signal(nil) = %v", err)
	}
}
