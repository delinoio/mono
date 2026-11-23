// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// damagedStateDir returns a state directory that holds an identity and a
// regular file where the runtime subdirectory images belongs, so that
// store.Open cannot create the layout.
func damagedStateDir(t *testing.T) string {
	t.Helper()
	dir := enrolledStateDir(t)
	for _, name := range []string{"identity.json", "images"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func assertEntriesGone(t *testing.T, dir string, names ...string) {
	t.Helper()
	for _, name := range names {
		if _, err := os.Lstat(filepath.Join(dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived the reset: %v", name, err)
		}
	}
}

// Reset deletes the identity and a regular file at images, which store.Open
// rejects, and exits 0 when the clean-up succeeds. A second reset on the
// emptied directory also exits 0.
func TestResetDamagedLayoutDeletesEverything(t *testing.T) {
	dir := damagedStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "process")
	for i := 0; i < 2; i++ {
		var out, errb bytes.Buffer
		if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitOK {
			t.Fatalf("reset %d: %d, stderr %q", i, code, errb.String())
		}
		assertEntriesGone(t, dir, "identity.json", "images", "marker")
	}
}

// A failed host clean-up still lets the local deletion run on a damaged
// layout, and the reset exits 1.
func TestResetDamagedLayoutHostFailureStillDeletesLocalData(t *testing.T) {
	dir := damagedStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	calls := stubCleanupHost(t, errors.New("nft: operation not permitted"))
	var out, errb bytes.Buffer
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitError {
		t.Fatalf("reset: %d, stderr %q", code, errb.String())
	}
	if *calls != 1 || !strings.Contains(errb.String(), "operation not permitted") {
		t.Fatalf("calls %d, stderr %q", *calls, errb.String())
	}
	assertEntriesGone(t, dir, "identity.json", "images")
}

// An absent state directory counts as empty: reset exits 0, still runs the
// host clean-up and does not create the directory.
func TestResetAbsentStateDirIsEmpty(t *testing.T) {
	enrolledStateDir(t)
	dir := filepath.Join(t.TempDir(), "absent")
	t.Setenv("OWNSAND_AGENT_STATE_DIR", dir)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	calls := stubCleanupHost(t, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitOK {
		t.Fatalf("reset: %d, stderr %q", code, errb.String())
	}
	if *calls != 1 {
		t.Fatalf("host clean-up ran %d times, want 1", *calls)
	}
	if _, err := os.Lstat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset created the absent state directory: %v", err)
	}
}

// A state directory that is a regular file is an invalid state directory:
// reset exits 1, leaves the file and still runs the host clean-up.
func TestResetStateDirThatIsAFileFails(t *testing.T) {
	enrolledStateDir(t)
	file := filepath.Join(t.TempDir(), "state")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OWNSAND_AGENT_STATE_DIR", file)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	calls := stubCleanupHost(t, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"reset", "--yes"}, nil, &out, &errb); code != exitError {
		t.Fatalf("reset: %d, stderr %q", code, errb.String())
	}
	if *calls != 1 {
		t.Fatalf("host clean-up ran %d times, want 1", *calls)
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatalf("state file changed: %v", err)
	}
}

// Reset without --yes leaves a damaged layout and the host alone.
func TestResetWithoutYesLeavesDamagedLayout(t *testing.T) {
	dir := damagedStateDir(t)
	t.Setenv("OWNSAND_AGENT_RUNTIME", "firecracker")
	calls := stubCleanupHost(t, nil)
	var out, errb bytes.Buffer
	if code := run([]string{"reset"}, nil, &out, &errb); code != exitUsage {
		t.Fatalf("reset: %d, stderr %q", code, errb.String())
	}
	if *calls != 0 {
		t.Fatalf("host clean-up ran %d times", *calls)
	}
	for _, name := range []string{"identity.json", "images"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s changed: %v", name, err)
		}
	}
}
