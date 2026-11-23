// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOpenServiceLogKeepsArchiveHeldByAnotherHandle reproduces the Windows
// failure that the portable tests stand in for: a handle without
// FILE_SHARE_DELETE (the share mode of os.Open) on the archive makes the
// rename of the log fail with a sharing violation. It needs no elevation.
func TestOpenServiceLogKeepsArchiveHeldByAnotherHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, logFile)
	archive := path + ".1"
	// One byte over the limit; Truncate extends the file with zeros.
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(logRotateBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, archive, "archive content")

	held, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	heldOpen := true
	t.Cleanup(func() {
		if heldOpen {
			_ = held.Close()
		}
	})

	first, err := openServiceLog(dir)
	if err != nil {
		t.Fatalf("openServiceLog with a held archive: %v", err)
	}
	if len(first.Notes) != 1 {
		t.Fatalf("got %d notes with a held archive, want 1", len(first.Notes))
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, archive); got != "archive content" {
		t.Fatalf("archive %q after the failed rotation", got)
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	heldOpen = false
	second, err := openServiceLog(dir)
	if err != nil {
		t.Fatalf("openServiceLog after the release: %v", err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if len(second.Notes) != 0 {
		t.Fatalf("got %d notes after the release, want none", len(second.Notes))
	}
	st, err := os.Stat(archive)
	if err != nil || st.Size() != logRotateBytes+1 {
		t.Fatalf("the log did not rotate to the archive: %v, %v", st, err)
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 0 {
		t.Fatalf("the new active log is not empty: %v, %v", st, err)
	}
}
