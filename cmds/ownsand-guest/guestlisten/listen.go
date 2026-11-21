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
//	hvsock://<service>  Hyper-V socket service GUID (not implemented yet)
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
		return net.Listen("unix", s.Address)
	case SchemeTCP:
		return net.Listen("tcp", s.Address)
	}
	return nil, fmt.Errorf("guestlisten: unknown scheme %q", s.Scheme)
}

// prepareUnixAddress clears the filesystem path of a unix listener. It
// removes only a stale socket file of an earlier run and never deletes any
// other kind of entry. A Linux abstract address (leading "@") has no
// filesystem entry, so it needs no cleanup. The returned errors never contain
// the path, because the caller logs them.
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
	if err := os.Remove(address); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("guestlisten: remove stale unix socket failed")
	}
	return nil
}

// listenHvsock is the plug-in point of the Hyper-V socket transport of
// Windows hosts.
//
// Not implemented yet: it needs an AF_HYPERV binding (for example
// github.com/Microsoft/go-winio) inside Windows guests and on Linux guests of
// Hyper-V hosts (where Hyper-V sockets appear as vsock). Remove this stub
// with the Windows runtime implementation.
func listenHvsock(string) (net.Listener, error) {
	return nil, fmt.Errorf("%w: Hyper-V sockets are not implemented yet", ErrUnsupported)
}
