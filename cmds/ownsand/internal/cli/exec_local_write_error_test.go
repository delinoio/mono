// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestExecTextStdoutWriteErrorExitsWithError checks that a failed write of the
// guest output to the local standard output exits with 1 and the write error
// on standard error, not as a data path failure.
func TestExecTextStdoutWriteErrorExitsWithError(t *testing.T) {
	r := newRunner(t)
	stdout := &failingWriter{failAfter: 0, err: errDiskFull}
	var stderr bytes.Buffer
	code := r.runWriters(stdout, &stderr, "sandbox", "exec", r.f.sb, "--", "echo", "hello")
	if code != ExitError {
		t.Fatalf("exit %d, want %d; stderr %q", code, ExitError, stderr.String())
	}
	if !strings.Contains(stderr.String(), errDiskFull.Error()) {
		t.Fatalf("stderr %q does not carry the write error", stderr.String())
	}
	if strings.Contains(stderr.String(), "data path") {
		t.Fatalf("stderr %q reports a data path failure", stderr.String())
	}
}

func TestExecTextStderrWriteErrorReportsDiagnostic(t *testing.T) {
	r := newRunner(t)
	var stdout bytes.Buffer
	stderr := &firstWriteFails{err: errDiskFull}
	code := r.runWriters(&stdout, stderr, "--no-color", "sandbox", "exec", r.f.sb, "--", "echo", "hello")
	if code != ExitError || stdout.String() != "hello\n" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout.String(), stderr.buf.String())
	}
	if want := "error: " + errDiskFull.Error() + "\n"; stderr.buf.String() != want {
		t.Fatalf("stderr %q, want %q", stderr.buf.String(), want)
	}
}

func TestExecTextStderrWriteErrorExitsWithoutDiagnostic(t *testing.T) {
	r := newRunner(t)
	var stdout bytes.Buffer
	stderr := &failingWriter{failAfter: 0, err: errDiskFull}
	code := r.runWriters(&stdout, stderr, "sandbox", "exec", r.f.sb, "--", "echo", "hello")
	if code != ExitError || stdout.String() != "hello\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout.String())
	}
}

// TestExecTextDataPathFailureStillExits255 checks that a genuine data path
// failure keeps exit 255 and its diagnostic, and that a successful exec still
// streams the guest output and returns the guest exit status.
func TestExecTextDataPathFailureStillExits255(t *testing.T) {
	r := newRunner(t)
	res := r.run("sandbox", "exec", r.f.sb, "--", "denied")
	if res.code != 255 || !strings.Contains(res.stderr, "data path: ") {
		t.Fatalf("denied: exit %d stderr %q", res.code, res.stderr)
	}
	res = r.run("sandbox", "exec", r.f.sb, "--", "echo", "hello")
	if res.code != 0 || res.stdout != "hello\n" || res.stderr != "err\xff\n" {
		t.Fatalf("echo: %+v", res)
	}
	res = r.run("sandbox", "exec", r.f.sb, "--", "exit3")
	if res.code != 3 {
		t.Fatalf("exit3: %+v", res)
	}
}
