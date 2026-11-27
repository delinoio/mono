// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestStatusReadWaitsForPublication(t *testing.T) {
	for _, code := range []windows.Errno{windows.ERROR_FILE_NOT_FOUND, windows.ERROR_SHARING_VIOLATION, windows.ERROR_ACCESS_DENIED} {
		t.Run(code.Error(), func(t *testing.T) {
			s := &Store{dir: t.TempDir()}
			path := s.Path(StatusFile)
			if err := s.SaveStatus("previous"); err != nil {
				t.Fatal(err)
			}
			publisher, err := openStatusLock(t.Context(), path, windows.GENERIC_WRITE, windows.OPEN_EXISTING)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if publisher != windows.InvalidHandle {
					_ = windows.CloseHandle(publisher)
				}
			}()
			if err := lockStatus(t.Context(), publisher, windows.LOCKFILE_EXCLUSIVE_LOCK); err != nil {
				t.Fatal(err)
			}
			initialRead := make(chan struct{})
			finished := make(chan error, 1)
			go func() {
				first := true
				data, err := readWindowsStatus(path, func(path string) ([]byte, error) {
					if first {
						first = false
						close(initialRead)
						return nil, &os.PathError{Op: "open", Path: path, Err: code}
					}
					return os.ReadFile(path)
				})
				var report string
				err = decodeJSON(path, data, err, &report)
				if err == nil && report != "next" {
					err = errors.New("read completed before the publisher released the lock")
				}
				finished <- err
			}()
			<-initialRead
			if err := WriteFileAtomic(path, []byte(`"next"`), 0o600); err != nil {
				t.Fatal(err)
			}
			unlockStatus(publisher)
			if err := windows.CloseHandle(publisher); err != nil {
				t.Fatal(err)
			}
			publisher = windows.InvalidHandle
			if err := <-finished; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStatusReadTimesOutDuringStuckPublication(t *testing.T) {
	path := filepath.Join(t.TempDir(), StatusFile)
	publisher, err := openStatusLock(t.Context(), path, windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(publisher)
	if err := lockStatus(t.Context(), publisher, windows.LOCKFILE_EXCLUSIVE_LOCK); err != nil {
		t.Fatal(err)
	}
	defer unlockStatus(publisher)
	reads := 0
	data, err := readWindowsStatus(path, func(path string) ([]byte, error) {
		reads++
		return os.ReadFile(path)
	})
	var report string
	err = decodeJSON(path, data, err, &report)
	if !errors.Is(err, context.DeadlineExceeded) || reads != 1 {
		t.Fatalf("read during stuck publication: %v after %d read calls", err, reads)
	}
}

func TestStatusReadPreservesGenuineAbsence(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenForRead(dir)
	if err != nil {
		t.Fatal(err)
	}
	checkAbsent := func() {
		t.Helper()
		var report string
		if err := s.LoadStatus(&report); !errors.Is(err, ErrNotFound) {
			t.Fatalf("absent report: %v", err)
		}
	}
	checkAbsent()
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("status must leave a fresh directory empty: %v, %v", entries, err)
	}
	// A previous publisher may have exited before its first successful move.
	lock, err := openStatusLock(t.Context(), s.Path(StatusFile), windows.GENERIC_WRITE, windows.OPEN_ALWAYS)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.CloseHandle(lock); err != nil {
		t.Fatal(err)
	}
	checkAbsent()
}

func TestStatusWriteCancellationKeepsPreviousReport(t *testing.T) {
	s := &Store{dir: t.TempDir()}
	if err := s.SaveStatus("previous"); err != nil {
		t.Fatal(err)
	}
	held := openHeld(t, s.Path(StatusFile), shareReadWrite)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	err := s.SaveStatusContext(ctx, "next")
	if closeErr := windows.CloseHandle(held); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("write with a conflicting handle: %v", err)
	}
	var report string
	if err := s.LoadStatus(&report); err != nil || report != "previous" {
		t.Fatalf("report after cancellation: %q, %v", report, err)
	}
	entries, err := filepath.Glob(s.Path(".status.json.tmp-*"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("temporary files after cancellation: %v, %v", entries, err)
	}
}

func TestStatusPublicationWithLongPaths(t *testing.T) {
	component := strings.Repeat("a", 100)
	dir := filepath.Join(t.TempDir(), component, component, component)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	s := &Store{dir: dir}
	for _, want := range []string{"first", "second"} {
		if err := s.SaveStatus(want); err != nil {
			t.Fatal(err)
		}
		var report string
		if err := s.LoadStatus(&report); err != nil || report != want {
			t.Fatalf("read report from long path: %q, %v", report, err)
		}
	}
}

func TestResetRemovesStatusLockWhileReaderHasItOpen(t *testing.T) {
	s := &Store{dir: t.TempDir()}
	path := s.Path(StatusFile)
	if err := s.SaveStatus("report"); err != nil {
		t.Fatal(err)
	}
	reader, err := openStatusLock(t.Context(), path, windows.GENERIC_READ, windows.OPEN_EXISTING)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Reset()
	if closeErr := windows.CloseHandle(reader); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{StatusFile, statusLockFile} {
		if _, err := os.Stat(s.Path(name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s after reset: %v", name, err)
		}
	}
}
