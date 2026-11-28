// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guestlisten

import (
	"errors"
	"os"
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

type fakeVsockRawConn struct {
	syscall.RawConn
	callbacks int
	waits     int
	waitErr   error
}

func (r *fakeVsockRawConn) Read(callback func(uintptr) bool) error {
	for {
		r.callbacks++
		if callback(12) {
			return nil
		}
		r.waits++
		if r.waitErr != nil {
			return r.waitErr
		}
	}
}

func TestVsockAccept(t *testing.T) {
	for _, tc := range []struct {
		name      string
		results   []error
		waitErr   error
		wantErr   error
		wantWaits int
		wantCalls int
	}{
		{name: "retry in the same callback", results: []error{unix.EINTR, unix.ECONNABORTED, nil}, wantCalls: 1},
		{name: "wait for readiness", results: []error{unix.ECONNABORTED, unix.EWOULDBLOCK, nil}, wantWaits: 1, wantCalls: 2},
		{name: "invalid listener", results: []error{unix.EINVAL}, wantErr: unix.EINVAL, wantCalls: 1},
		{name: "descriptor limit", results: []error{unix.EMFILE}, wantErr: unix.EMFILE, wantCalls: 1},
		{name: "closed during wait", results: []error{unix.EAGAIN}, waitErr: os.ErrClosed, wantErr: os.ErrClosed, wantWaits: 1, wantCalls: 1},
		{name: "setup interrupted", results: []error{acceptSetupError{unix.EINTR}}, wantErr: unix.EINTR, wantCalls: 1},
		{name: "setup aborted", results: []error{acceptSetupError{unix.ECONNABORTED}}, wantErr: unix.ECONNABORTED, wantCalls: 1},
		{name: "setup would block", results: []error{acceptSetupError{unix.EAGAIN}}, wantErr: unix.EAGAIN, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalAccept := acceptNonblock
			t.Cleanup(func() { acceptNonblock = originalAccept })
			attempts := 0
			acceptNonblock = func(fd int) (int, unix.Sockaddr, error) {
				if fd != 12 || attempts >= len(tc.results) {
					t.Fatalf("unexpected accept attempt %d on descriptor %d", attempts+1, fd)
				}
				err := tc.results[attempts]
				attempts++
				if err != nil {
					return -1, nil, err
				}
				return openDevNull(t), &unix.SockaddrVM{CID: 2, Port: 5000}, nil
			}
			raw := &fakeVsockRawConn{waitErr: tc.waitErr}
			local := &VsockAddr{CID: unix.VMADDR_CID_ANY, Port: 1024}
			listener := &vsockListener{raw: raw, addr: local}
			conn, err := listener.Accept()
			if conn != nil {
				defer conn.Close()
			}
			if err != tc.wantErr {
				t.Fatalf("Accept() error = %v, want %v", err, tc.wantErr)
			}
			if attempts != len(tc.results) || raw.callbacks != tc.wantCalls || raw.waits != tc.wantWaits {
				t.Fatalf("attempts = %d, callbacks = %d, waits = %d; want %d, %d, %d",
					attempts, raw.callbacks, raw.waits, len(tc.results), tc.wantCalls, tc.wantWaits)
			}
			if tc.wantErr != nil {
				if conn != nil {
					t.Fatal("Accept() returned a connection with an error")
				}
				return
			}
			if conn == nil {
				t.Fatal("Accept() returned no connection")
			}
			remote, ok := conn.RemoteAddr().(*VsockAddr)
			if !ok || *remote != (VsockAddr{CID: 2, Port: 5000}) {
				t.Errorf("remote address = %v, want vsock address 2:5000", conn.RemoteAddr())
			}
			if conn.LocalAddr() != local {
				t.Errorf("local address = %v, want %v", conn.LocalAddr(), local)
			}
		})
	}
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
