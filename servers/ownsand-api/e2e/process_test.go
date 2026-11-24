// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// envHelperMode makes the test binary act as a child process for the process
// tests below: it writes to both output streams and then exits or blocks.
const envHelperMode = "OWNSAND_E2E_HELPER_MODE"

const envHelperOutputHoldAddr = "OWNSAND_E2E_HELPER_OUTPUT_HOLD_ADDR"

// helperMode is the behavior of the helper process.
type helperMode string

const (
	// helperExit writes the output and exits with status 0.
	helperExit helperMode = "exit"
	// helperBlock writes the output and waits until it is signaled.
	helperBlock helperMode = "block"
	// helperSpawn starts a descendant with the same output descriptors.
	helperSpawn helperMode = "spawn"
	// helperHoldOutput writes the output and waits for the test to close its connection.
	helperHoldOutput helperMode = "hold-output"
)

// TestHelperProcess is the body of the child process, not a test. The final
// line of each stream has no trailing newline.
func TestHelperProcess(t *testing.T) {
	mode := helperMode(os.Getenv(envHelperMode))
	if mode == "" {
		t.Skip("only runs as a helper process")
	}
	if mode == helperSpawn {
		bin, args := helperArgs()
		cmd := exec.Command(bin, args...)
		cmd.Env = append(os.Environ(), envHelperMode+"="+string(helperHoldOutput))
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Run(); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	os.Stdout.WriteString("out-1\nout-final")
	os.Stderr.WriteString("err-1\nerr-final")
	if mode == helperBlock {
		time.Sleep(time.Minute)
	}
	if mode == helperHoldOutput {
		conn, err := net.DialTimeout("tcp4", os.Getenv(envHelperOutputHoldAddr), 10*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		// Dialing after the writes signals that diagnostic output is ready.
		_, _ = io.Copy(io.Discard, conn)
		conn.Close()
	}
	os.Exit(0)
}

func helperArgs() (string, []string) {
	return os.Args[0], []string{"-test.run=^TestHelperProcess$"}
}

func TestTypeScriptSDKLauncherSuccess(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	bin, args := helperArgs()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), envHelperMode+"="+string(helperExit))
	out, err := runTypeScriptSDKCommand(cmd)
	if err != nil {
		t.Fatalf("launcher failed: %v\n%s", err, out)
	}
	if got, want := string(out), "out-1\nout-finalerr-1\nerr-final"; got != want {
		t.Fatalf("combined output = %q, want %q", got, want)
	}
}

func TestTypeScriptSDKLauncherTimeout(t *testing.T) {
	listener, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	bin, args := helperArgs()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = append(os.Environ(), envHelperMode+"="+string(helperSpawn),
		envHelperOutputHoldAddr+"="+listener.Addr().String())
	type result struct {
		out []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, err := runTypeScriptSDKCommand(cmd)
		done <- result{out, err}
	}()

	conn, err := listener.Accept()
	if err != nil {
		t.Fatalf("descendant did not signal readiness: %v", err)
	}
	defer conn.Close()

	// Arm the short deadline after the descendant has inherited the pipe and
	// written its diagnostics. The launcher sees the same cancellation signal
	// as it does when the relay test's context deadline expires.
	deadline, stop := context.WithTimeout(t.Context(), 25*time.Millisecond)
	defer stop()
	<-deadline.Done()
	cancel()
	expiresAt, _ := deadline.Deadline()
	const schedulingTolerance = 2 * time.Second
	select {
	case got := <-done:
		var exitErr *exec.ExitError
		if !errors.As(got.err, &exitErr) {
			t.Fatalf("launcher error = %v, want a process exit error\n%s", got.err, got.out)
		}
		for _, want := range []string{"out-final", "err-final"} {
			if !strings.Contains(string(got.out), want) {
				t.Errorf("missing %q in combined output %q", want, got.out)
			}
		}
	case <-time.After(time.Until(expiresAt.Add(typeScriptSDKPipeDrainDelay + schedulingTolerance))):
		t.Fatal("launcher did not return within the pipe-drain delay and scheduling tolerance")
	}

	// The descendant keeps the pipe open until this connection is closed.
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var probe [1]byte
	if _, err := conn.Read(probe[:]); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("descendant stopped holding the pipe before the launcher returned: %v", err)
	}
}

func wantHelperLines(name string) []string {
	return []string{name + ": out-1", name + ": out-final", name + ": err-1", name + ": err-final"}
}

// assertHelperLines checks the lines of both streams, in any order across the
// streams.
func assertHelperLines(t *testing.T, logs *logSink, name string) {
	t.Helper()
	logs.mu.Lock()
	got := slices.Clone(logs.lines)
	logs.mu.Unlock()
	// A test binary may print its own lines after the helper output.
	for _, want := range wantHelperLines(name) {
		if !slices.Contains(got, want) {
			t.Errorf("missing log line %q in %q", want, got)
		}
	}
}

func assertNoScanners(t *testing.T, logs *logSink) {
	t.Helper()
	if n := logs.scanners.Load(); n != 0 {
		t.Errorf("%d log scanners still run", n)
	}
}

func TestStartProcessExitReleasesLogs(t *testing.T) {
	logs := &logSink{}
	bin, args := helperArgs()
	p, err := launchProcess(logs, "child", []string{envHelperMode + "=" + string(helperExit)}, bin, args...)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		t.Fatal("the process did not finish")
	}
	// The scanners are joined before done closes, so no waiting is needed.
	assertNoScanners(t, logs)
	assertHelperLines(t, logs, "child")
}

func TestStartProcessCleanupStopsAndReleasesLogs(t *testing.T) {
	logs := &logSink{}
	bin, args := helperArgs()
	var p *proc
	t.Run("run", func(t *testing.T) {
		p = startProcess(t, logs, "child", []string{envHelperMode + "=" + string(helperBlock)}, bin, args...)
		deadline := time.Now().Add(30 * time.Second)
		for !logs.hasLine("child: err-1") {
			if time.Now().After(deadline) {
				t.Fatal("the process wrote no output")
			}
			time.Sleep(10 * time.Millisecond)
		}
	})
	// The subtest cleanup stopped the process.
	select {
	case <-p.done:
	default:
		t.Fatal("the process is still running after the cleanup")
	}
	assertNoScanners(t, logs)
	assertHelperLines(t, logs, "child")
}

func TestStartProcessCrashReleasesLogs(t *testing.T) {
	logs := &logSink{}
	bin, args := helperArgs()
	var p *proc
	t.Run("run", func(t *testing.T) {
		p = startProcess(t, logs, "child", []string{envHelperMode + "=" + string(helperBlock)}, bin, args...)
		deadline := time.Now().Add(30 * time.Second)
		for !logs.hasLine("child: err-1") {
			if time.Now().After(deadline) {
				t.Fatal("the process wrote no output")
			}
			time.Sleep(10 * time.Millisecond)
		}
		killProcess(t, p)
		assertNoScanners(t, logs)
		assertHelperLines(t, logs, "child")
	})
	assertNoScanners(t, logs)
}

func TestStartProcessFailureReleasesLogs(t *testing.T) {
	logs := &logSink{}
	_, err := launchProcess(logs, "missing", nil, "/nonexistent/ownsand-e2e-binary")
	if err == nil {
		t.Fatal("starting a missing executable succeeded")
	}
	assertNoScanners(t, logs)
}

func TestLogSinkWriterRetainsFinalLine(t *testing.T) {
	logs := &logSink{}
	w, done := logs.writer("p")
	if _, err := w.Write([]byte("a\nb")); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	<-done
	logs.mu.Lock()
	defer logs.mu.Unlock()
	if want := []string{"p: a", "p: b"}; !slices.Equal(logs.lines, want) {
		t.Fatalf("lines = %q, want %q", logs.lines, want)
	}
}

func TestLogSinkScanErrorDoesNotBlockWriter(t *testing.T) {
	logs := &logSink{}
	w, done := logs.writer("p")
	// A line above the scanner limit stops the scan with an error.
	go func() { _, _ = w.Write(make([]byte, 2<<20)) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the scanner did not stop on a line that is too long")
	}
	// The reader is closed, so a later write fails instead of blocking.
	errc := make(chan error, 1)
	go func() { _, err := w.Write([]byte("more\n")); errc <- err }()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("write after the scan error succeeded")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("write blocked after the scan error")
	}
	_ = w.Close()
}

// TestDefaultLinuxGuestRuntime checks the runtime of the stacks that create
// LINUX sandboxes without selecting a runtime: the process runtime runs only
// guests of the host OS, so a macOS host uses the Virtualization.framework
// runtime when the base image is set and skips the test without it, instead
// of failing every create with failed_precondition.
func TestDefaultLinuxGuestRuntime(t *testing.T) {
	for _, tc := range []struct {
		goos, kernel, rootfs string
		want                 linuxGuestRuntime
		reason               string
	}{
		{goos: "linux", want: linuxGuestRuntimeProcess},
		{goos: "linux", kernel: "k", rootfs: "r", want: linuxGuestRuntimeProcess},
		{goos: "darwin", kernel: "k", rootfs: "r", want: linuxGuestRuntimeVirtualization},
		{goos: "darwin", want: linuxGuestRuntimeUnavailable, reason: envVZKernel},
		{goos: "darwin", kernel: "k", want: linuxGuestRuntimeUnavailable, reason: envVZRootfs},
		{goos: "windows", kernel: "k", rootfs: "r", want: linuxGuestRuntimeUnavailable, reason: "windows host"},
	} {
		got, reason := defaultLinuxGuestRuntime(tc.goos, tc.kernel, tc.rootfs)
		if got != tc.want || (tc.reason == "") != (reason == "") || !strings.Contains(reason, tc.reason) {
			t.Errorf("defaultLinuxGuestRuntime(%q, %q, %q) = (%v, %q), want (%v, containing %q)", tc.goos, tc.kernel, tc.rootfs, got, reason, tc.want, tc.reason)
		}
	}
}
