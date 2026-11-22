// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"bytes"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBufferLogger() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewTextHandler(&buf, nil)), &buf
}

// settledReadEntry returns an entry whose snapshot preparation has ended,
// because sweep never removes an entry that is still being prepared.
func settledReadEntry(snapshot string, lastEnd time.Time) *readEntry {
	ready := make(chan struct{})
	close(ready)
	return &readEntry{snapshot: snapshot, ready: ready, lastEnd: lastEnd}
}

func TestReadSweepFailureLogsNoPath(t *testing.T) {
	logger, buf := newBufferLogger()
	dir := filepath.Join(t.TempDir(), "secret-reads")
	m, err := newReadManager(dir, time.Now, logger)
	if err != nil {
		t.Fatal(err)
	}
	// A non-empty directory at the snapshot path makes os.Remove fail.
	snap := filepath.Join(dir, "secret-snapshot")
	if err := os.MkdirAll(snap, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(snap, "secret-file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	m.entries["read-1"] = settledReadEntry(snap, end)
	m.sweep(end.Add(m.retention))

	out := buf.String()
	if !strings.Contains(out, "remove read snapshot failed") || !strings.Contains(out, "read_id=read-1") {
		t.Fatalf("missing warning: %q", out)
	}
	for _, secret := range []string{"secret-reads", "secret-snapshot", "secret-file", dir} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q: %q", secret, out)
		}
	}
}

func TestReadSweepSuccessAndMissingLogNoWarning(t *testing.T) {
	logger, buf := newBufferLogger()
	dir := t.TempDir()
	m, err := newReadManager(dir, time.Now, logger)
	if err != nil {
		t.Fatal(err)
	}
	present := filepath.Join(dir, "present")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	end := time.Now()
	m.entries["a"] = settledReadEntry(present, end)
	m.entries["b"] = settledReadEntry(filepath.Join(dir, "missing"), end)
	m.sweep(end.Add(m.retention))
	if strings.Contains(buf.String(), "level=WARN") {
		t.Fatalf("unexpected warning: %q", buf.String())
	}
}

func TestWriteSweepFailureLogsNoPath(t *testing.T) {
	logger, buf := newBufferLogger()
	m, err := newWriteManager("", time.Now, logger)
	if err != nil {
		t.Fatal(err)
	}
	tmp := filepath.Join(t.TempDir(), "secret-dir", "secret-target.tmp")
	m.removeFile = func(path string) error {
		return &fs.PathError{Op: "remove", Path: path, Err: errors.New("device busy")}
	}
	last := time.Now()
	m.entries["write-1"] = &writeEntry{id: "write-1", tmp: tmp, lastChunk: last}
	m.sweep(last.Add(m.retention))

	out := buf.String()
	if !strings.Contains(out, "clean up write failed") || !strings.Contains(out, "write_id=write-1") ||
		!strings.Contains(out, "device busy") {
		t.Fatalf("missing warning: %q", out)
	}
	for _, secret := range []string{"secret-dir", "secret-target", tmp} {
		if strings.Contains(out, secret) {
			t.Fatalf("log leaks %q: %q", secret, out)
		}
	}
}
