//go:build linux

package vsock

import (
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

const (
	afVsock = 40 // AF_VSOCK
	// CIDAny is VMADDR_CID_ANY: listen on every context ID of the guest.
	CIDAny = 0xffffffff
)

// sockaddrVM is struct sockaddr_vm from <linux/vm_sockets.h>.
type sockaddrVM struct {
	family    uint16
	reserved1 uint16
	port      uint32
	cid       uint32
	flags     uint8
	zero      [3]uint8
}

// Addr is a vsock address.
type Addr struct {
	CID  uint32
	Port uint32
}

// Network returns "vsock".
func (a Addr) Network() string { return "vsock" }

func (a Addr) String() string { return fmt.Sprintf("vsock:%d:%d", a.CID, a.Port) }

// Listener accepts vsock connections.
type Listener struct {
	fd     int
	port   uint32
	mu     sync.Mutex
	closed bool
}

// Listen listens on port for every guest context ID.
func Listen(port uint32) (*Listener, error) {
	fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("vsock socket: %w", err)
	}
	sa := sockaddrVM{family: afVsock, port: port, cid: CIDAny}
	if _, _, errno := syscall.Syscall(syscall.SYS_BIND, uintptr(fd), uintptr(unsafe.Pointer(&sa)), unsafe.Sizeof(sa)); errno != 0 {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock bind: %w", errno)
	}
	if err := syscall.Listen(fd, 16); err != nil {
		_ = syscall.Close(fd)
		return nil, fmt.Errorf("vsock listen: %w", err)
	}
	return &Listener{fd: fd, port: port}, nil
}

// Accept waits for the next connection. syscall.Accept cannot be used,
// because it fails for socket address families that it does not know and
// closes the accepted socket.
func (l *Listener) Accept() (net.Conn, error) {
	for {
		nfd, _, errno := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(l.fd), 0, 0, syscall.SOCK_CLOEXEC, 0, 0)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			l.mu.Lock()
			closed := l.closed
			l.mu.Unlock()
			if closed {
				return nil, net.ErrClosed
			}
			return nil, fmt.Errorf("vsock accept: %w", errno)
		}
		if err := syscall.SetNonblock(int(nfd), true); err != nil {
			_ = syscall.Close(int(nfd))
			return nil, err
		}
		f := os.NewFile(nfd, "vsock")
		return &Conn{File: f, local: Addr{CID: CIDAny, Port: l.port}}, nil
	}
}

// Close stops the listener. shutdown wakes a blocked accept on Linux.
func (l *Listener) Close() error {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return nil
	}
	l.closed = true
	l.mu.Unlock()
	_ = syscall.Shutdown(l.fd, syscall.SHUT_RDWR)
	return syscall.Close(l.fd)
}

// Addr returns the listening address.
func (l *Listener) Addr() net.Addr { return Addr{CID: CIDAny, Port: l.port} }

// Conn is an accepted vsock connection. The non-blocking file uses the Go
// runtime poller, so deadlines work.
type Conn struct {
	*os.File
	local Addr
}

// LocalAddr returns the local address.
func (c *Conn) LocalAddr() net.Addr { return c.local }

// RemoteAddr returns the host address. The peer address is not queried.
func (c *Conn) RemoteAddr() net.Addr { return Addr{CID: 2, Port: 0} }

var _ net.Conn = (*Conn)(nil)
