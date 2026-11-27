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

func requireVsockSocket(t *testing.T) {
	t.Helper()

	// Keep the unmarked probe descriptor out of children started by os/exec.
	syscall.ForkLock.RLock()
	fd, err := unix.Socket(unix.AF_VSOCK, unix.SOCK_STREAM, 0)
	if err == nil {
		unix.Close(fd)
	}
	syscall.ForkLock.RUnlock()

	if err == nil {
		return
	}
	if reason := rawVsockSocketUnavailableReason(err); reason != "" {
		t.Skipf("raw AF_VSOCK stream socket %s: %v", reason, err)
	}
	t.Fatalf("open raw AF_VSOCK stream socket: %v", err)
}

func rawVsockSocketUnavailableReason(err error) string {
	switch err {
	case unix.EAFNOSUPPORT, unix.EPROTONOSUPPORT, unix.ESOCKTNOSUPPORT, unix.EOPNOTSUPP:
		return "unsupported"
	case unix.ENODEV:
		return "device unavailable"
	case unix.EACCES, unix.EPERM:
		return "denied"
	default:
		return ""
	}
}

func TestRawVsockSocketUnavailableReason(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want string
	}{
		{name: "available"},
		{name: "unsupported address family", err: unix.EAFNOSUPPORT, want: "unsupported"},
		{name: "unsupported protocol", err: unix.EPROTONOSUPPORT, want: "unsupported"},
		{name: "unsupported socket type", err: unix.ESOCKTNOSUPPORT, want: "unsupported"},
		{name: "unsupported operation", err: unix.EOPNOTSUPP, want: "unsupported"},
		{name: "missing device", err: unix.ENODEV, want: "device unavailable"},
		{name: "access denied", err: unix.EACCES, want: "denied"},
		{name: "operation denied", err: unix.EPERM, want: "denied"},
		{name: "unexpected I/O error", err: unix.EIO},
		// socket(2) takes no device path.
		{name: "unexpected missing path", err: unix.ENOENT},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := rawVsockSocketUnavailableReason(tt.err); got != tt.want {
				t.Errorf("reason for %v = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

func TestSocketCloexec(t *testing.T) {
	requireVsockSocket(t)
	fd, err := socketCloexec()
	if err != nil {
		t.Fatalf("socketCloexec: %v", err)
	}
	defer unix.Close(fd)
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("read listener socket descriptor flags: %v", err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatal("listener socket is not close-on-exec")
	}
}
