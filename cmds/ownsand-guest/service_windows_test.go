// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

func TestServiceRejectsInvalidLogLevel(t *testing.T) {
	t.Setenv(envLogLevel, "private-log-level")
	state := filepath.Join(t.TempDir(), "private-state")
	var events []string
	h := &serviceHandler{
		args: []string{"--state-dir", state, "--listen", "invalid://", "--clock", "ignore"},
		reportStartupError: func(message string) error {
			events = append(events, message)
			return errors.New("event log unavailable")
		},
	}
	specific, code := h.Execute(nil, nil, make(chan svc.Status, 2))
	if !specific || code != 2 || h.code != 2 {
		t.Fatalf("service-specific = %t, code = %d, process exit = %d; want true, 2, 2", specific, code, h.code)
	}
	const want = "Invalid log level. Set --log-level or OWNSAND_GUEST_LOG_LEVEL to debug, info, warn or error."
	if len(events) != 1 || events[0] != want {
		t.Fatalf("got events %q, want one event %q", events, want)
	}
	if _, err := os.Stat(state); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid log level reached log setup: stat state directory returned %v", err)
	}
}

func TestServiceRejectsInvalidClockMode(t *testing.T) {
	t.Setenv(envClock, "private-clock-mode")
	state := filepath.Join(t.TempDir(), "private-state")
	h := &serviceHandler{
		args: []string{"--state-dir", state, "--listen", "invalid://", "--log-level", "error"},
		reportStartupError: func(message string) error {
			t.Errorf("unexpected startup event: %s", message)
			return nil
		},
	}
	specific, code := h.Execute(nil, nil, make(chan svc.Status, 2))
	if !specific || code != 2 || h.code != 2 {
		t.Fatalf("service-specific = %t, code = %d, process exit = %d; want true, 2, 2", specific, code, h.code)
	}
	data := readFile(t, filepath.Join(state, logFile))
	records := logRecords(t, data)
	if len(records) != 1 || records[0]["level"] != "ERROR" || records[0]["component"] != "ownsand-guest" ||
		records[0]["msg"] != "Invalid clock mode. Set --clock or OWNSAND_GUEST_CLOCK to set or ignore." {
		t.Fatalf("unexpected service log: %v", records)
	}
	for _, value := range []string{"private-clock-mode", "private-state"} {
		if strings.Contains(data, value) {
			t.Fatalf("service log contains %q: %s", value, data)
		}
	}
}

func TestServiceLogSetupFailurePreservesExitCode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prepare   func(*testing.T, string)
		operation string
		code      windows.Errno
	}{
		{
			name: "mkdir",
			prepare: func(t *testing.T, state string) {
				writeFile(t, state, "occupied")
			},
			operation: "could not create the service state directory",
			code:      windows.ERROR_PATH_NOT_FOUND,
		},
		{
			name: "open",
			prepare: func(t *testing.T, state string) {
				if err := os.MkdirAll(filepath.Join(state, logFile), 0o700); err != nil {
					t.Fatal(err)
				}
			},
			operation: "could not open guest.log",
			code:      windows.ERROR_ACCESS_DENIED,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "private-state")
			tc.prepare(t, state)
			var events []string
			h := &serviceHandler{
				args: []string{"--state-dir", state, "--log-level", "info"},
				reportStartupError: func(message string) error {
					events = append(events, message)
					return errors.New("event log unavailable")
				},
			}
			specific, code := h.Execute(nil, nil, make(chan svc.Status, 2))
			if !specific || code != 1 || h.code != 1 {
				t.Fatalf("service-specific = %t, code = %d, process exit = %d; want true, 1, 1", specific, code, h.code)
			}
			want := fmt.Sprintf("%s (Windows error %d: %v)", tc.operation, tc.code, tc.code)
			if len(events) != 1 || events[0] != want || strings.Contains(events[0], "private-state") {
				t.Fatalf("got events %q, want one event %q", events, want)
			}
		})
	}
}

func TestServiceLogFailureOmitsUnknownCause(t *testing.T) {
	err := &logSetupError{operation: openGuestLog, cause: errors.New("private state path")}
	if got := serviceLogFailure(err); got != "could not open guest.log" {
		t.Fatalf("unexpected diagnostic %q", got)
	}
}

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

	h := &serviceHandler{reportStartupError: func(message string) error {
		t.Errorf("unexpected startup event: %s", message)
		return nil
	}}
	first, err := h.openServiceLog(dir)
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
	second, err := h.openServiceLog(dir)
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
