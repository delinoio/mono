// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

// stubCleanupHost replaces the host network clean-up of reset and counts its
// calls, so that no test touches the network of the machine that runs it.
func stubCleanupHost(t *testing.T, err error) *int {
	t.Helper()
	calls := new(int)
	prev := cleanupHost
	cleanupHost = func(*slog.Logger) error { *calls++; return err }
	t.Cleanup(func() { cleanupHost = prev })
	return calls
}

func enrolledStateDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("OWNSAND_AGENT_STATE_DIR", dir)
	t.Setenv("NO_COLOR", "1")
	if err := os.WriteFile(dir+"/marker", []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A failed host clean-up makes reset exit with an error that names the
// failure, and the local data is still deleted.
func TestResetHostFailureExitsNonzeroAndDeletesState(t *testing.T) {
	dir := enrolledStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	calls := stubCleanupHost(t, errors.New("delete tap device osb-00000000001: operation not permitted"))
	var out, errb bytes.Buffer
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitError {
		t.Fatalf("reset: %d, want %d", code, exitError)
	}
	if *calls != 1 || !strings.Contains(errb.String(), "osb-00000000001") {
		t.Fatalf("calls %d, stderr %q", *calls, errb.String())
	}
	if _, err := os.Stat(dir + "/marker"); !os.IsNotExist(err) {
		t.Fatalf("local data survived the reset: %v", err)
	}
}

// When the host clean-up succeeds (or every resource was already absent),
// reset exits 0; a retry after a failed reset succeeds.
func TestResetHostSuccessAndRetry(t *testing.T) {
	enrolledStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	var out, errb bytes.Buffer
	stubCleanupHost(t, errors.New("nft: operation not permitted"))
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitError {
		t.Fatalf("failing reset: %d", code)
	}
	calls := stubCleanupHost(t, nil)
	errb.Reset()
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitOK || *calls != 1 {
		t.Fatalf("retry: %d, calls %d, stderr %q", code, *calls, errb.String())
	}
}

// Reset without --yes touches neither the local data nor the host.
func TestResetWithoutYesLeavesEverything(t *testing.T) {
	dir := enrolledStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	calls := stubCleanupHost(t, errors.New("must not run"))
	var out, errb bytes.Buffer
	if code := run([]string{"reset"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("reset: %d", code)
	}
	if *calls != 0 {
		t.Fatalf("host clean-up ran %d times", *calls)
	}
	if _, err := os.Stat(dir + "/marker"); err != nil {
		t.Fatalf("local data changed: %v", err)
	}
}

// The process test runtime creates no host network resources, so reset does
// not clean up the host.
func TestResetProcessRuntimeSkipsHostCleanup(t *testing.T) {
	enrolledStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "process")
	calls := stubCleanupHost(t, errors.New("must not run"))
	var out, errb bytes.Buffer
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitOK || *calls != 0 {
		t.Fatalf("reset: %d, calls %d", code, *calls)
	}
}
