// SPDX-License-Identifier: Apache-2.0

//go:build windows

package credstore

import (
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"
	"unsafe"

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

func TestWindowsCredentialReadSerializesReplacement(t *testing.T) {
	s := Store{Dir: t.TempDir()}
	old := Credentials{SessionID: "session", RefreshToken: "old-refresh", AccessToken: "old-access"}
	next := Credentials{SessionID: "session", RefreshToken: "new-refresh", AccessToken: "new-access"}
	if err := s.Save(&old); err != nil {
		t.Fatal(err)
	}

	opened := make(chan struct{})
	resume := make(chan struct{})
	release := sync.OnceFunc(func() { close(resume) })
	defer release()
	type loadResult struct {
		credentials *Credentials
		err         error
	}
	loaded := make(chan loadResult, 1)
	go func() {
		c, err := s.loadWithLock(func(path string) ([]byte, error) {
			// Use the same Windows sharing mode as os.ReadFile. Hold the
			// handle past the replacement retry window before reading it.
			f, err := os.Open(path)
			if err != nil {
				return nil, err
			}
			defer f.Close()
			close(opened)
			<-resume
			return io.ReadAll(f)
		})
		loaded <- loadResult{credentials: c, err: err}
	}()
	select {
	case <-opened:
	case res := <-loaded:
		t.Fatalf("load returned before opening the credential file: %v", res.err)
	}

	started := make(chan struct{})
	saved := make(chan error, 1)
	go func() {
		close(started)
		saved <- s.SaveIfCurrent(old.SessionID, old.RefreshToken, &next)
	}()
	<-started
	select {
	case err := <-saved:
		t.Fatalf("refresh publication returned while the reader still held the file open: %v", err)
	case <-time.After(replaceRetryTimeout + 250*time.Millisecond):
	}
	release()
	if res := <-loaded; res.err != nil || *res.credentials != old {
		t.Fatalf("reader did not get the complete old credentials: %v", res.err)
	}
	if err := <-saved; err != nil {
		t.Fatalf("publish rotation after the read: %v", err)
	}
	if got, err := s.Load(); err != nil || *got != next {
		t.Fatalf("reader did not get the complete new credentials: %v", err)
	}
	assertUserOnlyDACL(t, s.Dir)
	assertUserOnlyDACL(t, s.Path())
}

func TestWindowsLoadWaitsForStoreLock(t *testing.T) {
	s := savedStore(t)
	unlock, err := s.lock()
	if err != nil {
		t.Fatal(err)
	}
	release := sync.OnceFunc(unlock)
	defer release()

	started := make(chan struct{})
	loaded := make(chan error, 1)
	go func() {
		close(started)
		_, err := s.Load()
		loaded <- err
	}()
	<-started
	select {
	case err := <-loaded:
		t.Fatalf("Load returned while the store lock was held: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()
	if err := <-loaded; err != nil {
		t.Fatalf("load after unlocking: %v", err)
	}
}

func assertUserOnlyDACL(t *testing.T, path string) {
	t.Helper()
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatal(err)
	}
	control, _, err := sd.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("credential store DACL is not protected: %v", err)
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil || dacl.AceCount != 1 {
		t.Fatalf("credential store DACL must have one entry: %v", err)
	}
	var ace *windows.ACCESS_ALLOWED_ACE
	if err := windows.GetAce(dacl, 0, &ace); err != nil {
		t.Fatal(err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
	if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE ||
		ace.Header.AceFlags&windows.INHERITED_ACE != 0 || !sid.Equals(user.User.Sid) {
		t.Fatal("credential store DACL must grant access only to the current user")
	}
}

func TestReadFileRetriesSharingViolation(t *testing.T) {
	s := savedStore(t)
	attempts := 0
	got, err := readFileWithRetry(s.Path(), func(path string) ([]byte, error) {
		attempts++
		if attempts == 1 {
			return nil, &os.PathError{Op: "open", Path: path, Err: windows.ERROR_SHARING_VIOLATION}
		}
		return os.ReadFile(path)
	})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) || attempts != 2 {
		t.Fatalf("want the saved credentials after two reads, got %d reads", attempts)
	}
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
