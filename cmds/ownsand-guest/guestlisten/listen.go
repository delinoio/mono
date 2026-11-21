// SPDX-License-Identifier: FSL-1.1-ALv2

// Package guestlisten opens the listener of the guest helper. The guest
// helper listens on the hypervisor socket of the platform (vsock on
// Firecracker-class microVMs and on macOS hosts, a Hyper-V socket on Windows
// hosts) and never on a guest network interface. The test runtime of the
// device agent runs the guest helper as a host process and uses a unix
// socket or a loopback TCP listener instead.
//
// A listener spec is a URL-like string:
//
//	vsock://:1024       vsock port 1024, any CID
//	hvsock://<service>  Hyper-V socket service GUID (Windows guests)
//	unix:///path/sock   unix domain socket
//	tcp://127.0.0.1:0   TCP, for tests only
package guestlisten

import (
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Scheme names the transport of a listener spec.
type Scheme string

const (
	SchemeVsock  Scheme = "vsock"
	SchemeHvsock Scheme = "hvsock"
	SchemeUnix   Scheme = "unix"
	SchemeTCP    Scheme = "tcp"
)

// DefaultVsockPort is the vsock port of the guest helper. The device agent
// connects to this port.
const DefaultVsockPort uint32 = 1024

// DefaultSpec is the listener of the guest helper inside a sandbox guest.
var DefaultSpec = fmt.Sprintf("vsock://:%d", DefaultVsockPort)

// ErrUnsupported reports a transport that this platform does not provide.
var ErrUnsupported = errors.New("guestlisten: transport not supported on this platform")

// ErrAddressOccupied reports a unix listener path that holds an entry other
// than a socket. The listener never deletes such an entry. The message omits
// the path.
var ErrAddressOccupied = errors.New("guestlisten: unix socket path is occupied by a non-socket entry")

// ErrAddressInUse reports a unix listener path that a live listener serves.
// The listener never unlinks the socket file of a live listener, so the
// earlier listener keeps accepting connections. The message omits the path.
var ErrAddressInUse = errors.New("guestlisten: unix socket path is served by a live listener")

// ErrProbeFailed reports that the listener could not tell whether the socket
// file at its unix path is stale. It leaves the file in place. The message
// omits the path.
var ErrProbeFailed = errors.New("guestlisten: probe unix socket failed")

// ErrBindFailed reports a failed bind of a unix socket. The message names
// the operating system cause but omits the socket path, which the OS error of
// the standard library would repeat.
var ErrBindFailed = errors.New("guestlisten: bind unix socket failed")

// unixProbeTimeout bounds the connection attempt that tells a live listener
// from a stale socket file.
const unixProbeTimeout = time.Second

// Spec is a parsed listener spec.
type Spec struct {
	Scheme Scheme
	// Address is the unix socket path, the TCP host:port or the Hyper-V
	// service ID.
	Address string
	// Port is the vsock port.
	Port uint32
}

// Parse parses a listener spec.
func Parse(spec string) (Spec, error) {
	scheme, rest, ok := strings.Cut(spec, "://")
	if !ok || rest == "" {
		return Spec{}, fmt.Errorf("guestlisten: spec %q must look like scheme://address", spec)
	}
	s := Spec{Scheme: Scheme(scheme), Address: rest}
	switch s.Scheme {
	case SchemeVsock:
		portText := strings.TrimPrefix(rest, ":")
		port, err := strconv.ParseUint(portText, 10, 32)
		if err != nil || port == 0 {
			return Spec{}, fmt.Errorf("guestlisten: invalid vsock port %q", portText)
		}
		s.Port = uint32(port)
		s.Address = ""
	case SchemeUnix, SchemeTCP, SchemeHvsock:
	default:
		return Spec{}, fmt.Errorf("guestlisten: unknown scheme %q", scheme)
	}
	return s, nil
}

// Listen opens the listener that spec names.
func Listen(spec string) (net.Listener, error) {
	s, err := Parse(spec)
	if err != nil {
		return nil, err
	}
	switch s.Scheme {
	case SchemeVsock:
		return listenVsock(s.Port)
	case SchemeHvsock:
		return listenHvsock(s.Address)
	case SchemeUnix:
		if err := prepareUnixAddress(s.Address); err != nil {
			return nil, err
		}
		l, err := net.Listen("unix", s.Address)
		if err != nil {
			return nil, bindError(err)
		}
		return l, nil
	case SchemeTCP:
		return net.Listen("tcp", s.Address)
	}
	return nil, fmt.Errorf("guestlisten: unknown scheme %q", s.Scheme)
}

// prepareUnixAddress clears the filesystem path of a unix listener. It
// removes only a stale socket file of an earlier run: a socket file that a
// live listener serves is never removed, because a new bind would take the
// path from that listener. It never deletes any other kind of entry. A Linux
// abstract address (leading "@") has no filesystem entry, so it needs no
// cleanup. The returned errors never contain the path, because the caller
// logs them.
func prepareUnixAddress(address string) error {
	if runtime.GOOS == "linux" && strings.HasPrefix(address, "@") {
		return nil
	}
	info, err := os.Lstat(address)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("guestlisten: inspect unix socket path failed")
	}
	if info.Mode().Type() != os.ModeSocket {
		return ErrAddressOccupied
	}
	// Probe the socket: a connection that succeeds, or that fails for any
	// reason other than "nobody listens", means a live listener (or an
	// unknown state), and the file stays.
	conn, err := net.DialTimeout("unix", address, unixProbeTimeout)
	if err == nil {
		_ = conn.Close()
		return ErrAddressInUse
	}
	if !isStaleSocketDialError(err) {
		return pathFreeError(ErrProbeFailed, err)
	}
	// Remove only the entry that the probe found. When another process
	// replaced it meanwhile, leave the new entry alone; the bind then fails
	// without taking the path of a live listener.
	current, err := os.Lstat(address)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("guestlisten: inspect unix socket path failed")
	}
	if !os.SameFile(info, current) {
		return ErrAddressInUse
	}
	if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("guestlisten: remove stale unix socket failed")
	}
	return nil
}

// windowsConnRefused is WSAECONNREFUSED. The syscall package of Windows
// defines ECONNREFUSED as an invented value that a Winsock error never
// carries, so the Windows build matches the Winsock code itself.
const windowsConnRefused syscall.Errno = 10061

// isStaleSocketDialError reports whether a failed connection attempt to a
// socket file proves that no listener serves it: the connection was refused
// (the file remains after its listener ended) or the file vanished.
func isStaleSocketDialError(err error) bool {
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	return runtime.GOOS == "windows" && errors.Is(err, windowsConnRefused)
}

// bindError converts the error of a failed unix bind to an error without the
// socket path. EADDRINUSE means that another listener won the path after the
// probe, which is the same condition as a live listener.
func bindError(err error) error {
	if errors.Is(err, syscall.EADDRINUSE) {
		return ErrAddressInUse
	}
	return pathFreeError(ErrBindFailed, err)
}

// pathFreeError returns an error that matches kind and, when err carries an
// operating system error number, that number (for errors.Is and errors.As).
// Its text is kind plus the text of that number, never the text of err,
// because the error of the net package repeats the socket path.
func pathFreeError(kind, err error) error {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return kind
	}
	return &causedError{kind: kind, errno: errno}
}

type causedError struct {
	kind  error
	errno syscall.Errno
}

func (e *causedError) Error() string { return e.kind.Error() + ": " + e.errno.Error() }

func (e *causedError) Unwrap() []error { return []error{e.kind, e.errno} }

// listenHvsock opens a Hyper-V socket listener on a service GUID. Windows
// guests of the Hyper-V runtime set it (hvsock_windows.go); a Linux guest of
// a Hyper-V host sees Hyper-V sockets as vsock and uses vsock:// instead.
var listenHvsock = func(string) (net.Listener, error) {
	return nil, fmt.Errorf("%w: Hyper-V sockets need a Windows guest", ErrUnsupported)
}
