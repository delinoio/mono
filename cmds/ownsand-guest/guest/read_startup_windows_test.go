// SPDX-License-Identifier: FSL-1.1-ALv2

package guest

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

func TestReadStartupHeldSnapshotLogsNoPath(t *testing.T) {
	t.Parallel()
	const stateMarker = "private-state"
	const snapshotMarker = "snapshots-private-old"
	const fileMarker = "private-held-file"
	state := filepath.Join(t.TempDir(), stateMarker)
	root := filepath.Join(state, "reads")
	old := filepath.Join(root, snapshotMarker)
	heldPath := filepath.Join(old, fileMarker)
	held := holdReadStartupFile(t, heldPath)
	if err := os.Remove(heldPath); !errors.Is(err, windows.ERROR_SHARING_VIOLATION) {
		t.Fatalf("native removal of file without delete sharing: %v, want ERROR_SHARING_VIOLATION (%d)", err, windows.ERROR_SHARING_VIOLATION)
	}
	t.Logf("native deletion conflict: ERROR_SHARING_VIOLATION (%d)", windows.ERROR_SHARING_VIOLATION)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	cfg := Config{StateDir: state, Logger: logger}
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	if first.reads.dir == old || filepath.Dir(first.reads.dir) != root {
		t.Fatalf("new snapshot directory %q is not distinct from %q under the same root", first.reads.dir, old)
	}
	if info, err := os.Stat(first.reads.dir); err != nil || !info.IsDir() {
		t.Fatalf("new snapshot directory was not created: %v", err)
	}
	if _, err := os.Stat(heldPath); err != nil {
		t.Fatalf("old snapshot file after startup: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	var warning map[string]string
	if err := json.Unmarshal(logs.Bytes(), &warning); err != nil {
		t.Fatalf("expected exactly one JSON warning: %v", err)
	}
	const message = "old read snapshot directory is still in use and stays until the next start"
	if warning["level"] != "WARN" || warning["msg"] != message {
		t.Fatalf("unexpected startup warning: %v", warning)
	}
	if cause := warning["error"]; cause != windows.ERROR_SHARING_VIOLATION.Error() {
		t.Errorf("unexpected removal reason: %q", cause)
	}
	if path, ok := warning["path"]; ok {
		t.Errorf("startup warning contains a path attribute: %q", path)
	}
	for _, marker := range []string{stateMarker, snapshotMarker, fileMarker} {
		if strings.Contains(logs.String(), marker) {
			t.Errorf("startup warning leaks %q: %s", marker, &logs)
		}
	}

	if err := held.Close(); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	second, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old snapshot directory after restarting with the handle closed: %v, want not exist", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("unexpected warning after restarting with the handle closed: %s", &logs)
	}
}

func TestWindowsReadSnapshotCleanupHeldFileOutsideSnapshotFails(t *testing.T) {
	t.Parallel()
	state := t.TempDir()
	root := filepath.Join(state, "reads")
	holdReadStartupFile(t, filepath.Join(root, "unexpected-file"))
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))

	assertReadStartupCleanupFailure(t, Config{StateDir: state, Logger: logger}, windows.ERROR_SHARING_VIOLATION, "unexpected-file")
	if logs.Len() != 0 {
		t.Fatalf("unexpected warning for fatal cleanup error: %s", &logs)
	}
}

func holdReadStartupFile(t *testing.T, path string) *os.File {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	// Deny deletion until the test closes this handle.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	file := os.NewFile(uintptr(handle), path)
	t.Cleanup(func() { _ = file.Close() })
	return file
}
