// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

type failingHelpWriter struct {
	output       bytes.Buffer
	writesBefore int
	failures     int
	err          error
}

func (w *failingHelpWriter) Write(p []byte) (int, error) {
	if w.writesBefore == 0 {
		w.failures++
		return 0, w.err
	}
	w.writesBefore--
	return w.output.Write(p)
}

func TestHelpAndVersionWriteErrors(t *testing.T) {
	writeErr := errors.New("stdout write rejected")
	for _, tc := range []struct {
		name         string
		args         []string
		writesBefore int
	}{
		{name: "help command", args: []string{"help"}},
		{name: "root short flag", args: []string{"-h"}},
		{name: "root long flag", args: []string{"--help"}},
		{name: "root help after a global flag", args: []string{"--no-color", "--help"}},
		{name: "org list help", args: []string{"org", "list", "--help"}},
		{name: "session exec help", args: []string{"session", "exec", "--help"}},
		{name: "version", args: []string{"version"}},
		{name: "root command list", args: []string{"help"}, writesBefore: 1},
		{name: "subcommand flag list", args: []string{"org", "list", "--help"}, writesBefore: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &failingHelpWriter{writesBefore: tc.writesBefore, err: writeErr}
			var stderr bytes.Buffer
			code := Run(context.Background(), Env{
				Args: tc.args, Stdout: stdout, Stderr: &stderr,
				LookupEnv: func(key string) (string, bool) { return "", key == EnvNoColor },
			})
			if code != ExitError || stderr.String() != "error: "+writeErr.Error()+"\n" {
				t.Fatalf("exit = %d, stderr = %q, want exit %d and the write error", code, stderr.String(), ExitError)
			}
			if stdout.failures != 1 {
				t.Errorf("failed writes = %d, want one", stdout.failures)
			}
			if tc.writesBefore > 0 && !strings.HasPrefix(stdout.output.String(), "Usage: tide ") {
				t.Errorf("partial stdout = %q, want usage prefix", stdout.output.String())
			}
		})
	}
}

func TestHelpAndVersionOutput(t *testing.T) {
	for _, tc := range []struct {
		name   string
		args   []string
		prefix string
		suffix string
	}{
		{name: "root help", args: []string{"help"}, prefix: "Usage: tide [global flags]", suffix: "file of a --*-file flag, never from command-line arguments.\n"},
		{name: "subcommand help", args: []string{"org", "list", "--help"}, prefix: "Usage: tide org list [flags]\n\nFlags:\n", suffix: "write structured debug logs to standard error\n"},
		{name: "version", args: []string{"version"}, prefix: "tide " + Version + "\n", suffix: "tide " + Version + "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run(context.Background(), Env{Args: tc.args, Stdout: &stdout, Stderr: &stderr})
			if code != ExitOK || stderr.Len() != 0 {
				t.Fatalf("exit = %d, stderr = %q, want exit %d and empty stderr", code, stderr.String(), ExitOK)
			}
			if got := stdout.String(); !strings.HasPrefix(got, tc.prefix) || !strings.HasSuffix(got, tc.suffix) {
				t.Errorf("stdout = %q, want prefix %q and suffix %q", got, tc.prefix, tc.suffix)
			}
		})
	}
}

func TestNoArgumentsPrintsUsageToStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), Env{Stdout: &stdout, Stderr: &stderr})
	if code != ExitUsage || stdout.Len() != 0 || !strings.HasPrefix(stderr.String(), "Usage: tide [global flags]") {
		t.Fatalf("exit = %d, stdout = %q, stderr = %q, want exit %d and usage on stderr", code, stdout.String(), stderr.String(), ExitUsage)
	}
}
