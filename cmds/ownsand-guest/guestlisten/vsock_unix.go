// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package guestlisten

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// VsockAddr is the address of a vsock endpoint.
type VsockAddr struct {
	CID  uint32
	Port uint32
}

func (a *VsockAddr) Network() string { return "vsock" }
func (a *VsockAddr) String() string {
	return strconv.FormatUint(uint64(a.CID), 10) + ":" + strconv.FormatUint(uint64(a.Port), 10)
}

type vsockListener struct {
	file *os.File
	raw  syscall.RawConn
	addr *VsockAddr
	once sync.Once
}

// setCloseOnExec marks fd close-on-exec. It is a variable so that tests can
// inject a failure.
var setCloseOnExec = func(fd int) error {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_SETFD, unix.FD_CLOEXEC)
	return err
}

// createCloexec runs create, which makes a new file descriptor, and marks the
// descriptor close-on-exec while it holds syscall.ForkLock for reading.
//
// macOS has no SOCK_CLOEXEC and no accept4, so the descriptor is created
// first and marked afterwards. Without the lock, an os/exec fork between the
// two steps copies the descriptor into the child. os/exec takes ForkLock for
// writing around fork, so holding it for reading makes creation and marking
// atomic with respect to every fork of this process. When the mark fails, the
// descriptor is closed and the error is returned.
//
// create must not block: a reader that waits holds off every pending fork.
// Callers pass a non-blocking listener, so accept returns EAGAIN at once.
func createCloexec(create func() (int, error), mark func(int) error) (int, error) {
	syscall.ForkLock.RLock()
	defer syscall.ForkLock.RUnlock()
	fd, err := create()
	if err != nil {
		return -1, err
	}
	if err := mark(fd); err != nil {
		unix.Close(fd)
		return -1, fmt.Errorf("guestlisten: mark close-on-exec: %w", err)
	}
	return fd, nil
}

func listenVsock(port uint32) (net.Listener, error) {
	fd, err := socketCloexec()
	if err != nil {
		return nil, fmt.Errorf("guestlisten: vsock socket: %w", err)
	}
	// The listener must stay non-blocking: acceptNonblock holds
	// syscall.ForkLock for reading around each accept on macOS, and a
	// blocking accept would hold it until a peer connects and stall every
	// os/exec start in the process.
	if err := unix.SetNonblock(fd, true); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("guestlisten: vsock nonblock: %w", err)
	}
	if err := unix.Bind(fd, &unix.SockaddrVM{CID: unix.VMADDR_CID_ANY, Port: port}); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("guestlisten: vsock bind port %d: %w", port, err)
	}
	if err := unix.Listen(fd, 128); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("guestlisten: vsock listen: %w", err)
	}
	f := os.NewFile(uintptr(fd), "vsock-listener")
	raw, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &vsockListener{file: f, raw: raw, addr: &VsockAddr{CID: unix.VMADDR_CID_ANY, Port: port}}, nil
}

func (l *vsockListener) Accept() (net.Conn, error) {
	var (
		nfd       int
		peer      unix.Sockaddr
		acceptErr error
	)
	err := l.raw.Read(func(fd uintptr) bool {
		nfd, peer, acceptErr = acceptNonblock(int(fd))
		return !errors.Is(acceptErr, unix.EAGAIN)
	})
	if err != nil {
		return nil, err
	}
	if acceptErr != nil {
		return nil, acceptErr
	}
	remote := &VsockAddr{}
	if vm, ok := peer.(*unix.SockaddrVM); ok {
		remote.CID, remote.Port = vm.CID, vm.Port
	}
	return &vsockConn{File: os.NewFile(uintptr(nfd), "vsock-conn"), local: l.addr, remote: remote}, nil
}

func (l *vsockListener) Close() error {
	var err error
	l.once.Do(func() { err = l.file.Close() })
	return err
}

func (l *vsockListener) Addr() net.Addr { return l.addr }

// vsockConn is a net.Conn over a non-blocking vsock file descriptor. The
// os.File uses the runtime poller, so deadlines work.
type vsockConn struct {
	*os.File
	local, remote *VsockAddr
}

func (c *vsockConn) LocalAddr() net.Addr  { return c.local }
func (c *vsockConn) RemoteAddr() net.Addr { return c.remote }
func (c *vsockConn) SetDeadline(t time.Time) error {
	return c.File.SetDeadline(t)
}
