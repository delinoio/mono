// SPDX-License-Identifier: Apache-2.0

//go:build windows

package credstore

import (
	"errors"
	"os"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

// holdWithoutDeleteShare opens path so that deleting it fails with a sharing
// violation, as an antivirus scanner does. The handle closes once.
func holdWithoutDeleteShare(t *testing.T, path string) (release func()) {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil,
		windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatalf("hold the credential file: %v", err)
	}
	released := false
	release = func() {
		if !released {
			released = true
			_ = windows.CloseHandle(h)
		}
	}
	t.Cleanup(release)
	return release
}

// stubRemoveRetry replaces the retry seams for one test. sleep runs instead of
// a real pause, so the tests do not depend on timer granularity.
func stubRemoveRetry(t *testing.T, timeout time.Duration, sleep func(time.Duration)) {
	t.Helper()
	oldTimeout, oldSleep := removeRetryTimeout, removeRetrySleep
	removeRetryTimeout, removeRetrySleep = timeout, sleep
	t.Cleanup(func() { removeRetryTimeout, removeRetrySleep = oldTimeout, oldSleep })
}

func savedStore(t *testing.T) Store {
	t.Helper()
	s := Store{Dir: t.TempDir()}
	if err := s.Save(&Credentials{RefreshToken: "r"}); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestDeleteRetriesUntilHandleIsReleased(t *testing.T) {
	s := savedStore(t)
	release := holdWithoutDeleteShare(t, s.Path())
	sleeps := 0
	// The first attempt fails while the handle is open; the handle closes
	// during the first pause, so the next attempt succeeds.
	stubRemoveRetry(t, time.Hour, func(time.Duration) {
		sleeps++
		release()
	})
	if err := s.Delete(); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if sleeps != 1 {
		t.Fatalf("want one retry pause, got %d", sleeps)
	}
	if _, err := os.Stat(s.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credential file must be gone, stat error: %v", err)
	}
}

func TestDeleteReturnsLastErrorAfterTimeout(t *testing.T) {
	s := savedStore(t)
	holdWithoutDeleteShare(t, s.Path())
	stubRemoveRetry(t, 20*time.Millisecond, func(time.Duration) {})
	err := s.Delete()
	if err == nil {
		t.Fatal("delete must fail while the file stays open")
	}
	if !isTransientReplaceError(err) {
		t.Fatalf("delete must return the last removal error, got a different error type")
	}
	if _, err := os.Stat(s.Path()); err != nil {
		t.Fatalf("credentials must stay when the delete fails: %v", err)
	}
	// The lock is released: another holder can take it.
	unlock, err := s.lock()
	if err != nil {
		t.Fatalf("lock must be free after a failed delete: %v", err)
	}
	unlock()
}

func TestRemoveFileMissingFileIsSuccess(t *testing.T) {
	stubRemoveRetry(t, time.Hour, func(time.Duration) { t.Error("must not pause") })
	if err := removeFile(t.TempDir() + `\absent.json`); err != nil {
		t.Fatalf("a missing file is success: %v", err)
	}
}

func TestRemoveFileOtherErrorReturnsImmediately(t *testing.T) {
	stubRemoveRetry(t, time.Hour, func(time.Duration) { t.Error("must not pause") })
	// A path with an invalid character fails with an error that is neither
	// transient nor not-exist.
	err := removeFile(t.TempDir() + "\\bad\x00name")
	if err == nil {
		t.Fatal("want an error")
	}
	if isTransientReplaceError(err) || errors.Is(err, os.ErrNotExist) {
		t.Fatal("the error must be neither transient nor not-exist")
	}
}
