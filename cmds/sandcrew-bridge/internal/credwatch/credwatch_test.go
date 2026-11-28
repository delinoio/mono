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
	w.Run(ctx, Handlers{
		Change: func(uint64) { t.Error("change reported") },
		Error:  func(error) { errorsSeen++ },
	})
	if errorsSeen != 1 {
		t.Fatalf("errors reported %d times, want once", errorsSeen)
	}
}

func TestRunChecksAfterNotifications(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The interval check never runs in this test, so only a notification
	// finds the change.
	w, err := New(path, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	w.debounce = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	changes := make(chan uint64, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, Handlers{
			Change:      func(revision uint64) { changes <- revision },
			Error:       func(err error) { t.Errorf("check failed: %v", err) },
			PollingOnly: func(err error) { t.Errorf("no file system notifications: %v", err) },
		})
	}()
	// Notifications start in Run; a write before that is not reported, so
	// write until the first change arrives.
	deadline := time.After(5 * time.Second)
	for revision := uint64(0); revision == 0; {
		if err := os.WriteFile(path, []byte("two"), 0o600); err != nil {
			t.Fatal(err)
		}
		select {
		case revision = <-changes:
		case <-time.After(100 * time.Millisecond):
		case <-deadline:
			t.Fatal("no change reported after a notification")
		}
	}
	// A rename over the file is a change too.
	tmp := filepath.Join(filepath.Dir(path), "auth.json.tmp")
	if err := os.WriteFile(tmp, []byte("three"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	select {
	case revision := <-changes:
		if revision != 2 {
			t.Fatalf("revision %d, want 2", revision)
		}
	case <-deadline:
		t.Fatal("no change reported after a rename")
	}
	cancel()
	<-done
}
