// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCLIHelpStdout(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "ownsand")
	if output, err := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build ownsand: %v\n%s", err, output)
	}

	runWithBrokenStdout := func(t *testing.T, wantExit int, args ...string) string {
		t.Helper()
		reader, writer, err := os.Pipe()
		if err != nil {
			t.Fatalf("create stdout pipe: %v", err)
		}
		defer writer.Close()
		if err := reader.Close(); err != nil {
			t.Fatalf("close stdout reader: %v", err)
		}

		var stderr bytes.Buffer
		cmd := exec.CommandContext(t.Context(), binary, args...)
		cmd.Stdout = writer
		cmd.Stderr = &stderr
		var exitErr *exec.ExitError
		if err := cmd.Run(); !errors.As(err, &exitErr) || exitErr.ExitCode() != wantExit {
			t.Fatalf("ownsand %v: %v, want normal exit %d (stderr %q)", args, err, wantExit, stderr.String())
		}
		return stderr.String()
	}

	const rootUsage = "Usage: ownsand [global flags]"
	for _, tc := range []struct {
		name  string
		args  []string
		usage string
	}{
		{"help command", []string{"help"}, rootUsage},
		{"short flag", []string{"-h"}, rootUsage},
		{"long flag", []string{"--help"}, rootUsage},
		{"long flag after global flag", []string{"--no-color", "--help"}, rootUsage},
		{"subcommand help", []string{"sandbox", "list", "--help"}, "Usage: ownsand sandbox list"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if stderr := runWithBrokenStdout(t, 1, tc.args...); !strings.Contains(stderr, unix.EPIPE.Error()) {
				t.Errorf("stderr %q, want %q", stderr, unix.EPIPE.Error())
			}

			var stderr bytes.Buffer
			cmd := exec.CommandContext(t.Context(), binary, tc.args...)
			cmd.Stderr = &stderr
			stdout, err := cmd.Output()
			if err != nil || !bytes.HasPrefix(stdout, []byte(tc.usage)) || stderr.Len() != 0 {
				t.Fatalf("ownsand %v: %v, stdout %q, stderr %q; want exit 0 and %q on stdout", tc.args, err, stdout, stderr.String(), tc.usage)
			}
		})
	}

	t.Run("usage with no arguments", func(t *testing.T) {
		if stderr := runWithBrokenStdout(t, 2); !strings.HasPrefix(stderr, rootUsage) {
			t.Errorf("stderr %q, want %q", stderr, rootUsage)
		}
	})
}
