package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

const (
	rootHelpTail = "Authentication: OWNSAND_API_KEY selects an organization API key; otherwise\n" +
		"the CLI uses the tokens that 'ownsand login' stored.\n"
	listHelpTail   = "  -verbose\n    \twrite structured logs to standard error\n"
	createHelpTail = "  -wait\n    \twait until the sandbox is running, paused, failed or deleted (default true)\n"
)

// createHelpFailingWriter records earlier output and rejects the final flag write.
type createHelpFailingWriter struct {
	accepted bytes.Buffer
	rejected string
}

func (w *createHelpFailingWriter) Write(p []byte) (int, error) {
	text := string(p)
	if text == createHelpTail {
		w.rejected = text
		return 0, errDiskFull
	}
	return w.accepted.Write(p)
}

// TestHelpWriteErrorExitsWithError checks that a failed help write exits
// with 1 and reports the writer error on standard error.
func TestHelpWriteErrorExitsWithError(t *testing.T) {
	checkWriteError := func(t *testing.T, r *runner, stdout io.Writer, args ...string) {
		t.Helper()
		var stderr bytes.Buffer
		if code := r.runWriters(stdout, &stderr, args...); code != ExitError {
			t.Errorf("exit %d, want %d (stderr %q)", code, ExitError, stderr.String())
		}
		if !strings.Contains(stderr.String(), errDiskFull.Error()) {
			t.Errorf("stderr misses the writer error: %q", stderr.String())
		}
		if strings.Contains(stderr.String(), "Run 'ownsand help' for usage.") {
			t.Errorf("stderr contains a usage hint: %q", stderr.String())
		}
	}

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
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &failingWriter{failAfter: tc.failAfter, err: errDiskFull}
			checkWriteError(t, newRunner(t), stdout, tc.args...)
		})
	}

	t.Run("subcommand help fails at the last flag", func(t *testing.T) {
		r := newRunner(t)
		args := []string{"sandbox", "create", "--help"}
		stdout := &createHelpFailingWriter{}
		checkWriteError(t, r, stdout, args...)
		if stdout.rejected != createHelpTail {
			t.Errorf("rejected write %q, want %q", stdout.rejected, createHelpTail)
		}
		wantAccepted, found := strings.CutSuffix(r.run(args...).stdout, createHelpTail)
		if !found || stdout.accepted.Len() == 0 || stdout.accepted.String() != wantAccepted {
			t.Errorf("accepted output %q, want all help before the final flag: %q", stdout.accepted.String(), wantAccepted)
		}
	})
}

// TestHelpWriteSucceedsWithUnlimitedWriter checks that help reaches its
// expected ending and exits with 0 when standard output accepts every write.
func TestHelpWriteSucceedsWithUnlimitedWriter(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		tail string
	}{
		{"root help", []string{"--help"}, rootHelpTail},
		{"sandbox list help", []string{"sandbox", "list", "-h"}, listHelpTail},
		{"sandbox create help", []string{"sandbox", "create", "--help"}, createHelpTail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRunner(t)
			stdout := &failingWriter{failAfter: -1}
			var stderr bytes.Buffer
			if code := r.runWriters(stdout, &stderr, tc.args...); code != ExitOK {
				t.Fatalf("exit %d, want %d (stderr %q)", code, ExitOK, stderr.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr %q, want empty", stderr.String())
			}
			output := stdout.buf.String()
			if !strings.HasPrefix(output, "Usage: ownsand") {
				t.Errorf("stdout %q, want the usage", output)
			}
			if !strings.HasSuffix(output, tc.tail) {
				t.Errorf("stdout %q, want it to end with %q", output, tc.tail)
			}
		})
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
