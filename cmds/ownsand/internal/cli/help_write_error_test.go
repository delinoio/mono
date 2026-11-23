package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestHelpWriteErrorExitsWithError checks that a failed write of the root
// help or of a subcommand help exits with 1 and the writer error on standard
// error, as a failed write of a result does, and that the help of a working
// standard output is unchanged.
func TestHelpWriteErrorExitsWithError(t *testing.T) {
	for _, tc := range []struct {
		name      string
		args      []string
		failAfter int
	}{
		{"help command", []string{"help"}, 0},
		{"short flag", []string{"-h"}, 0},
		{"long flag", []string{"--help"}, 0},
		{"long flag after a global flag", []string{"--no-color", "--help"}, 0},
		{"usage fails after the first chunk", []string{"--help"}, 1},
		{"usage fails in the command list", []string{"help"}, 2},
		{"subcommand help fails in the header", []string{"sandbox", "list", "-h"}, 0},
		{"subcommand help fails in the flag list", []string{"sandbox", "list", "-h"}, 1},
		{"subcommand help fails at the last flag", []string{"sandbox", "create", "--help"}, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(t)
			var stderr bytes.Buffer
			stdout := &failingWriter{failAfter: tc.failAfter, err: errDiskFull}
			if code := r.runWriters(stdout, &stderr, tc.args...); code != ExitError {
				t.Fatalf("exit %d, want %d (stderr %q)", code, ExitError, stderr.String())
			}
			if !strings.Contains(stderr.String(), errDiskFull.Error()) {
				t.Fatalf("stderr misses the writer error: %q", stderr.String())
			}
			if strings.Contains(stderr.String(), "Run 'ownsand help' for usage.") {
				t.Fatalf("a failed help write is not a usage error: %q", stderr.String())
			}
		})
	}
}

// TestHelpWriteSucceedsWithUnlimitedWriter checks that the help that fails
// above in the middle of its text is complete and exits with 0 when standard
// output accepts every write.
func TestHelpWriteSucceedsWithUnlimitedWriter(t *testing.T) {
	r := newRunner(t)
	for _, args := range [][]string{{"--help"}, {"sandbox", "list", "-h"}, {"sandbox", "create", "--help"}} {
		stdout := &failingWriter{failAfter: -1}
		var stderr bytes.Buffer
		if code := r.runWriters(stdout, &stderr, args...); code != ExitOK {
			t.Fatalf("%v: exit %d, want %d (stderr %q)", args, code, ExitOK, stderr.String())
		}
		if stderr.Len() != 0 {
			t.Errorf("%v: stderr %q, want empty", args, stderr.String())
		}
		if !strings.HasPrefix(stdout.buf.String(), "Usage: ownsand") {
			t.Errorf("%v: stdout %q, want the usage", args, stdout.buf.String())
		}
	}
}

// TestNoArgumentsUsageStillExitsWithUsageStatus checks that the usage error
// of a run without a command keeps the usage on standard error and status 2.
func TestNoArgumentsUsageStillExitsWithUsageStatus(t *testing.T) {
	r := newRunner(t)
	res := r.run()
	if res.code != ExitUsage || res.stdout != "" || !strings.HasPrefix(res.stderr, "Usage: ownsand [global flags]") {
		t.Fatalf("no arguments = (%d, %q, %q)", res.code, res.stdout, res.stderr)
	}
}
