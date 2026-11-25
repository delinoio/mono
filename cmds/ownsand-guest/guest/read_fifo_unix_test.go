// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build unix

package guest

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fifoOpenTimeout bounds how long a test waits for a snapshot of a swapped
// source. The base behavior blocks forever in open(2) on a FIFO without a
// writer, so a failure of the fix shows as this timeout and not as a hang of
// the whole test binary.
const fifoOpenTimeout = 5 * time.Second

// swapForFIFO returns an openHook that replaces the regular file at path with
// a FIFO once, between the first stat and the open of the source.
func swapForFIFO(t *testing.T, path string) func() {
	t.Helper()
	swapped := false
	return func() {
		if swapped {
			return
		}
		swapped = true
		if err := os.Remove(path); err != nil {
			t.Error(err)
			return
		}
		if err := syscall.Mkfifo(path, 0o644); err != nil {
			t.Error(err)
		}
	}
}

// A regular file that is replaced by a FIFO between the stat and the open must
// not block the open: the snapshot fails with the abort error instead.
func TestMakeSnapshotAbortsWhenPathSwappedForFIFO(t *testing.T) {
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "f")
	for _, forceCopy := range []bool{true, false} {
		if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
			t.Fatal(err)
		}
		m.forceCopy = forceCopy
		m.openHook = swapForFIFO(t, path)
		done := make(chan error, 1)
		go func() {
			_, _, _, err := m.makeSnapshot(path, filepath.Join(m.dir, "snap"))
			done <- err
		}()
		select {
		case err := <-done:
			if err == nil || !strings.Contains(err.Error(), "file changed during the snapshot copy") {
				t.Fatalf("forceCopy=%v: expected abort, got %v", forceCopy, err)
			}
		case <-time.After(fifoOpenTimeout):
			t.Fatalf("forceCopy=%v: makeSnapshot blocked on the FIFO", forceCopy)
		}
		if _, err := os.Lstat(filepath.Join(m.dir, "snap")); err == nil {
			t.Fatalf("forceCopy=%v: a snapshot file was created for the FIFO", forceCopy)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

// The prepare worker must publish the failure, so that ready is closed and a
// retry of the read_id snapshots again, when the source became a FIFO.
func TestReadPrepareClosesReadyWhenPathSwappedForFIFO(t *testing.T) {
	m, err := newReadManager(filepath.Join(t.TempDir(), "reads"), time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.close(time.Second) })
	m.forceCopy = true
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("aaaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.openHook = swapForFIFO(t, path)
	e, err := m.begin("r1", path)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-e.ready:
	case <-time.After(fifoOpenTimeout):
		t.Fatal("prepare did not close ready for a source swapped for a FIFO")
	}
	if e.err == nil || !strings.Contains(e.err.Error(), "file changed during the snapshot copy") {
		t.Fatalf("expected abort, got %v", e.err)
	}
	m.end("r1", e)
	m.mu.Lock()
	_, kept := m.entries["r1"]
	m.mu.Unlock()
	if kept {
		t.Fatal("the failed entry was not dropped")
	}
}
