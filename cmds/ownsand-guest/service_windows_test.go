// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
)

const (
	systemVolume = "\\\\?\\Volume{11111111-1111-1111-1111-111111111111}\\"
	dataVolume   = "\\\\?\\Volume{22222222-2222-2222-2222-222222222222}\\"
)

type flushVolumeFixture struct {
	guid             string
	mounts           []string
	driveType        uint32
	unavailableMount string
	mountErr         error
	openErr          error
	flushErr         error
}

func TestFlushVolumesSelectsMountedFixedVolumes(t *testing.T) {
	const removableVolume = "\\\\?\\Volume{33333333-3333-3333-3333-333333333333}\\"
	const unmountedVolume = "\\\\?\\Volume{44444444-4444-4444-4444-444444444444}\\"
	const dataRoot = "C:\\mounts\\data\\"
	// These two aliases push the MULTI_SZ past MAX_PATH without requiring
	// a single mount path to exceed MAX_PATH.
	alias := "C:\\mounts\\データ\\" + strings.Repeat("folder\\", 18)
	unavailableAlias := alias + "a\\"
	volumes := []flushVolumeFixture{
		{guid: systemVolume, mounts: []string{"C:\\", "D:\\system\\"}, driveType: windows.DRIVE_FIXED},
		{guid: dataVolume, mounts: []string{unavailableAlias, dataRoot, alias + "b\\"}, driveType: windows.DRIVE_FIXED, unavailableMount: unavailableAlias},
		{guid: removableVolume, mounts: []string{"E:\\"}, driveType: windows.DRIVE_REMOVABLE},
		{guid: unmountedVolume},
	}
	code, diagnostics, calls := runFlushVolumeFixture(t, volumes, nil, windows.ERROR_NO_MORE_FILES)
	if code != 0 || diagnostics != "" {
		t.Fatalf("flush exited with %d, stderr = %q", code, diagnostics)
	}
	assertFlushCalls(t, calls, []string{
		"list " + systemVolume, "type C:\\", "open " + strings.TrimSuffix(systemVolume, "\\"),
		"flush " + systemVolume, "close " + systemVolume,
		"list " + dataVolume, "resize", "list " + dataVolume, "type " + unavailableAlias, "type " + dataRoot,
		"open " + strings.TrimSuffix(dataVolume, "\\"), "flush " + dataVolume, "close " + dataVolume,
		"list " + removableVolume, "type E:\\", "list " + unmountedVolume, "close enumeration",
	})
}

func TestFlushVolumesContinuesAfterVolumeErrors(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failure    flushVolumeFixture
		diagnostic string
		calls      []string
	}{
		{
			name: "mount lookup", failure: flushVolumeFixture{mountErr: windows.ERROR_ACCESS_DENIED},
			diagnostic: "list mount paths for " + systemVolume,
			calls:      []string{"list " + systemVolume},
		},
		{
			name: "open", failure: flushVolumeFixture{openErr: windows.ERROR_ACCESS_DENIED},
			diagnostic: "flush " + systemVolume + ": open volume",
			calls:      []string{"list " + systemVolume, "type C:\\", "open " + strings.TrimSuffix(systemVolume, "\\")},
		},
		{
			name: "flush", failure: flushVolumeFixture{flushErr: windows.ERROR_ACCESS_DENIED},
			diagnostic: "flush " + systemVolume + ": FlushFileBuffers",
			calls: []string{
				"list " + systemVolume, "type C:\\", "open " + strings.TrimSuffix(systemVolume, "\\"),
				"flush " + systemVolume, "close " + systemVolume,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := tc.failure
			first.guid, first.mounts, first.driveType = systemVolume, []string{"C:\\"}, windows.DRIVE_FIXED
			volumes := []flushVolumeFixture{
				first,
				{guid: dataVolume, mounts: []string{"C:\\mounts\\data\\"}, driveType: windows.DRIVE_FIXED},
			}
			code, diagnostics, calls := runFlushVolumeFixture(t, volumes, nil, windows.ERROR_NO_MORE_FILES)
			if code != 1 || !strings.Contains(diagnostics, tc.diagnostic+": "+windows.ERROR_ACCESS_DENIED.Error()) {
				t.Fatalf("flush exited with %d, stderr = %q, want %q with ERROR_ACCESS_DENIED", code, diagnostics, tc.diagnostic)
			}
			assertFlushCalls(t, calls, append(slices.Clone(tc.calls),
				"list "+dataVolume, "type C:\\mounts\\data\\", "open "+strings.TrimSuffix(dataVolume, "\\"),
				"flush "+dataVolume, "close "+dataVolume, "close enumeration"))
		})
	}
}

func TestFlushVolumesReportsEnumerationErrors(t *testing.T) {
	t.Run("first", func(t *testing.T) {
		code, diagnostics, calls := runFlushVolumeFixture(t, nil, windows.ERROR_ACCESS_DENIED, nil)
		if code != 1 || !strings.Contains(diagnostics, "list volumes: "+windows.ERROR_ACCESS_DENIED.Error()) {
			t.Fatalf("flush exited with %d, stderr = %q", code, diagnostics)
		}
		assertFlushCalls(t, calls, nil)
	})
	t.Run("next", func(t *testing.T) {
		code, diagnostics, calls := runFlushVolumeFixture(t, []flushVolumeFixture{{guid: systemVolume}}, nil, windows.ERROR_ACCESS_DENIED)
		if code != 1 || !strings.Contains(diagnostics, "list volumes: "+windows.ERROR_ACCESS_DENIED.Error()) {
			t.Fatalf("flush exited with %d, stderr = %q", code, diagnostics)
		}
		assertFlushCalls(t, calls, []string{"list " + systemVolume, "close enumeration"})
	})
}

func runFlushVolumeFixture(t *testing.T, volumes []flushVolumeFixture, firstErr, endErr error) (int, string, []string) {
	t.Helper()
	const enumHandle, volumeHandle windows.Handle = 1, 2
	var calls []string
	current := 0
	writeName := func(dst *uint16, capacity uint32) {
		name := utf16.Encode([]rune(volumes[current].guid + "\x00"))
		if copy(unsafe.Slice(dst, capacity), name) != len(name) {
			t.Fatal("volume name buffer is too small")
		}
	}
	checkHandle := func(got, want windows.Handle) {
		if got != want {
			t.Fatalf("handle = %d, want %d", got, want)
		}
	}
	api := volumeAPI{
		findFirst: func(name *uint16, capacity uint32) (windows.Handle, error) {
			if firstErr != nil {
				return windows.InvalidHandle, firstErr
			}
			writeName(name, capacity)
			return enumHandle, nil
		},
		findNext: func(h windows.Handle, name *uint16, capacity uint32) error {
			checkHandle(h, enumHandle)
			if current+1 == len(volumes) {
				return endErr
			}
			current++
			writeName(name, capacity)
			return nil
		},
		findClose: func(h windows.Handle) error {
			checkHandle(h, enumHandle)
			calls = append(calls, "close enumeration")
			return nil
		},
		getMountPaths: func(name, dst *uint16, capacity uint32, length *uint32) error {
			guid := windows.UTF16PtrToString(name)
			volume := volumes[current]
			if guid != volume.guid {
				t.Fatalf("mount lookup for %q, want %q", guid, volume.guid)
			}
			calls = append(calls, "list "+guid)
			if volume.mountErr != nil {
				return volume.mountErr
			}
			paths := utf16.Encode([]rune(strings.Join(volume.mounts, "\x00") + "\x00\x00"))
			*length = uint32(len(paths))
			if copy(unsafe.Slice(dst, capacity), paths) != len(paths) {
				calls = append(calls, "resize")
				return windows.ERROR_MORE_DATA
			}
			return nil
		},
		getDriveType: func(root *uint16) uint32 {
			path := windows.UTF16PtrToString(root)
			calls = append(calls, "type "+path)
			volume := volumes[current]
			if !slices.Contains(volume.mounts, path) {
				t.Fatalf("drive type requested for unexpected root %q", path)
			}
			if path == volume.unavailableMount {
				return windows.DRIVE_NO_ROOT_DIR
			}
			return volume.driveType
		},
		open: func(path *uint16, access, share uint32, sa *windows.SecurityAttributes, disposition, flags uint32, template windows.Handle) (windows.Handle, error) {
			calls = append(calls, "open "+windows.UTF16PtrToString(path))
			if access != windows.GENERIC_READ|windows.GENERIC_WRITE || share != windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE ||
				sa != nil || disposition != windows.OPEN_EXISTING || flags != 0 || template != 0 {
				t.Fatal("unexpected CreateFile arguments")
			}
			if err := volumes[current].openErr; err != nil {
				return windows.InvalidHandle, err
			}
			return volumeHandle, nil
		},
		flush: func(h windows.Handle) error {
			checkHandle(h, volumeHandle)
			calls = append(calls, "flush "+volumes[current].guid)
			return volumes[current].flushErr
		},
		close: func(h windows.Handle) error {
			checkHandle(h, volumeHandle)
			calls = append(calls, "close "+volumes[current].guid)
			return nil
		},
	}
	var stderr bytes.Buffer
	code := api.flushMountedFixedVolumes(&stderr)
	return code, stderr.String(), calls
}

func assertFlushCalls(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("flush calls = %q, want %q", got, want)
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
