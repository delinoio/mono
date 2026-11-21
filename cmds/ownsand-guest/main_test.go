// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// runCapturingStderr runs run with args and returns its exit code and what
// it wrote to os.Stderr.
func runCapturingStderr(t *testing.T, args []string) (int, string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	orig := os.Stderr
	os.Stderr = w
	code := run(args)
	os.Stderr = orig
	w.Close()
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()
	return code, string(out)
}

func TestRunRejectsInvalidLogLevel(t *testing.T) {
	for _, level := range []string{"ınfo", "ınfo+1", "info+1", "warn-2", "İNFO", "verbose"} {
		// An invalid level must stop run before it starts a listener, so
		// an unreachable listen spec makes any later step fail visibly.
		code, stderr := runCapturingStderr(t, []string{"--log-level", level, "--listen", "unix://" + t.TempDir() + "/guest.sock"})
		if code != 2 {
			t.Errorf("log level %q: exit code %d, want 2", level, code)
		}
		want := "ownsand-guest: invalid log level \"" + level + "\"\n"
		if stderr != want {
			t.Errorf("log level %q: stderr %q, want %q", level, stderr, want)
		}
	}
}

func TestRunAcceptsASCIICaseFoldedLogLevel(t *testing.T) {
	// An invalid clock mode makes run return 2 right after the level check,
	// so the test reaches the step after level parsing without starting a
	// listener.
	for _, level := range []string{"DEBUG", "Warn", "Error", "iNfO"} {
		code, stderr := runCapturingStderr(t, []string{"--log-level", level, "--clock", "bogus"})
		if code != 2 {
			t.Errorf("log level %q: exit code %d, want 2", level, code)
		}
		if strings.Contains(stderr, "invalid log level") {
			t.Errorf("log level %q rejected: %q", level, stderr)
		}
		if !strings.Contains(stderr, "invalid clock mode") {
			t.Errorf("log level %q: stderr %q does not reach the clock check", level, stderr)
		}
	}
}
