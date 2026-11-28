// SPDX-License-Identifier: Apache-2.0

package credwatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCheckReportsContentChangesOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	w, err := New(path, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	check := func(wantChanged bool, wantRevision uint64) {
		t.Helper()
		changed, revision, err := w.Check()
		if err != nil || changed != wantChanged || revision != wantRevision {
			t.Fatalf("Check() = %v, %d, %v; want %v, %d", changed, revision, err, wantChanged, wantRevision)
		}
	}
	check(false, 0)
	os.WriteFile(path, []byte("two"), 0o600)
	check(true, 1)
	later := time.Now().Add(time.Hour)
	os.Chtimes(path, later, later)
	check(false, 1)
	os.Remove(path)
	check(false, 1)
	os.WriteFile(path, []byte("two"), 0o600)
	check(false, 1)
	os.WriteFile(path, []byte("three"), 0o600)
	check(true, 2)
}

func TestRunReportsTheFirstErrorOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	os.WriteFile(path, []byte("one"), 0o600)
	w, err := New(path, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	// A directory in place of the file fails every read.
	os.Remove(path)
	os.Mkdir(path, 0o700)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	errorsSeen := 0
	w.Run(ctx, func(uint64) { t.Error("change reported") }, func(error) { errorsSeen++ })
	if errorsSeen != 1 {
		t.Fatalf("errors reported %d times, want once", errorsSeen)
	}
}
