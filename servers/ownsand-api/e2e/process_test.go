// SPDX-License-Identifier: FSL-1.1-ALv2

package e2e

import (
	"os"
	"slices"
	"testing"
	"time"
)

// envHelperMode makes the test binary act as a child process for the process
// tests below: it writes to both output streams and then exits or blocks.
const envHelperMode = "OWNSAND_E2E_HELPER_MODE"

// helperMode is the behavior of the helper process.
type helperMode string

const (
	// helperExit writes the output and exits with status 0.
	helperExit helperMode = "exit"
	// helperBlock writes the output and waits until it is signaled.
	helperBlock helperMode = "block"
)

// TestHelperProcess is the body of the child process, not a test. The final
// line of each stream has no trailing newline.
func TestHelperProcess(t *testing.T) {
	mode := helperMode(os.Getenv(envHelperMode))
	if mode == "" {
		t.Skip("only runs as a helper process")
	}
	os.Stdout.WriteString("out-1\nout-final")
	os.Stderr.WriteString("err-1\nerr-final")
	if mode == helperBlock {
		time.Sleep(time.Minute)
	}
	os.Exit(0)
}

func helperArgs() (string, []string) {
	return os.Args[0], []string{"-test.run=^TestHelperProcess$"}
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
