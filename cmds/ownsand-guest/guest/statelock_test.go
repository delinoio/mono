// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNewRejectsSecondServerOnStateDir(t *testing.T) {
	dir := t.TempDir()
	first, err := New(Config{StateDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	// A retained read snapshot of the running helper.
	snapshot := filepath.Join(dir, "reads", "snapshot")
	if err := os.WriteFile(snapshot, []byte("kept"), 0o600); err != nil {
		t.Fatal(err)
	}

	second, err := New(Config{StateDir: dir})
	if err == nil {
		second.Close()
		t.Fatal("a second server started on a state directory in use")
	}
	if !errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("error %v, want ErrStateDirInUse", err)
	}
	if strings.Contains(err.Error(), dir) {
		t.Fatalf("error %q contains the state directory path", err)
	}
	if data, err := os.ReadFile(snapshot); err != nil || string(data) != "kept" {
		t.Fatalf("snapshot of the running helper after the rejected start: %q, %v", data, err)
	}

	// After the first server released the directory, a replacement starts
	// and clears the snapshots of the earlier helper.
	first.Close()
	first.Close() // Close is idempotent.
	replacement, err := New(Config{StateDir: dir})
	if err != nil {
		t.Fatalf("replacement after Close: %v", err)
	}
	defer replacement.Close()
	if _, err := os.Stat(snapshot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete snapshot survived the replacement: %v", err)
	}
}

func TestNewReleasesStateDirAfterFailedStart(t *testing.T) {
	dir := t.TempDir()
	// A regular file where the write records directory belongs makes the
	// start fail after the lock was taken.
	blocker := filepath.Join(dir, "writes")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if srv, err := New(Config{StateDir: dir}); err == nil {
		srv.Close()
		t.Fatal("start succeeded with a file in place of the write records directory")
	} else if errors.Is(err, ErrStateDirInUse) {
		t.Fatalf("unexpected lock error: %v", err)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	srv, err := New(Config{StateDir: dir})
	if err != nil {
		t.Fatalf("start after a failed start: %v", err)
	}
	srv.Close()
}
