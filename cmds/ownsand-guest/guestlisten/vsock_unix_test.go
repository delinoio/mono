// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guestlisten

import (
	"errors"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

// forkLockHeldForReading reports whether syscall.ForkLock is held by a reader:
// a writer cannot take it then.
func forkLockHeldForReading() bool {
	if syscall.ForkLock.TryLock() {
		syscall.ForkLock.Unlock()
		return false
	}
	return true
}

func openDevNull(t *testing.T) int {
	t.Helper()
	fd, err := unix.Open("/dev/null", unix.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	return fd
}

func fdOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

func TestCreateCloexecHoldsForkLockAroundCreateAndMark(t *testing.T) {
	var createLocked, markLocked bool
	var created int
	fd, err := createCloexec(func() (int, error) {
		createLocked = forkLockHeldForReading()
		created = openDevNull(t)
		return created, nil
	}, func(fd int) error {
		markLocked = forkLockHeldForReading()
		return setCloseOnExec(fd)
	})
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if fd != created {
		t.Fatalf("fd = %d, want %d", fd, created)
	}
	if !createLocked || !markLocked {
		t.Fatalf("ForkLock held for reading: create=%v mark=%v, want both true", createLocked, markLocked)
	}
	if forkLockHeldForReading() {
		t.Fatal("ForkLock still held after createCloexec returned")
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("descriptor is not close-on-exec")
	}
}

func TestCreateCloexecMarkFailureClosesDescriptor(t *testing.T) {
	markErr := errors.New("fcntl failed")
	var created int
	fd, err := createCloexec(func() (int, error) {
		created = openDevNull(t)
		return created, nil
	}, func(int) error { return markErr })
	if !errors.Is(err, markErr) {
		t.Fatalf("err = %v, want %v", err, markErr)
	}
	if fd != -1 {
		t.Fatalf("fd = %d, want -1", fd)
	}
	if fdOpen(created) {
		unix.Close(created)
		t.Fatal("descriptor still open after a failed mark")
	}
	if forkLockHeldForReading() {
		t.Fatal("ForkLock still held after a failed mark")
	}
}

func TestCreateCloexecCreateFailureSkipsMark(t *testing.T) {
	createErr := unix.EAGAIN
	marked := false
	fd, err := createCloexec(func() (int, error) { return -1, createErr }, func(int) error {
		marked = true
		return nil
	})
	if !errors.Is(err, createErr) {
		t.Fatalf("err = %v, want %v", err, createErr)
	}
	if fd != -1 || marked {
		t.Fatalf("fd = %d, marked = %v; want -1, false", fd, marked)
	}
	if forkLockHeldForReading() {
		t.Fatal("ForkLock still held after a failed create")
	}
}

// TestSocketCloexec checks the socket of the listener is close-on-exec on
// every platform; it skips when the host has no vsock.
func TestSocketCloexec(t *testing.T) {
	fd, err := socketCloexec()
	if err != nil {
		t.Skipf("vsock unavailable: %v", err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatal(err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("listener socket is not close-on-exec")
	}
}
