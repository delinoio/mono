// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"

	"github.com/delinoio/mono/cmds/ownsand-guest/guestlisten"
)

// tunnelParam is the kernel command-line parameter of the network tunnel:
// `ownsand.tunnel=<vsock port>,<guest address>/<prefix bits>,<gateway>`.
// The Hyper-V runtime attaches no network device to the guest: it sets this
// parameter for OUTBOUND sandboxes, and the guest helper then creates the
// TAP device eth0 and carries its Ethernet frames over a hypervisor socket
// to the agent's userspace network stack, which applies the network policy.
const tunnelParam = "ownsand.tunnel="

// tunnelInterface is the name of the TAP device of the tunnel.
const tunnelInterface = "eth0"

// maxFrame bounds one tunnel frame (an Ethernet frame of the 1500-byte MTU
// with headroom).
const maxFrame = 65535

type tunnelSpec struct {
	port    uint32
	guest   netip.Prefix
	gateway netip.Addr
}

func parseTunnelSpec(cmdline string) (tunnelSpec, bool, error) {
	for _, field := range strings.Fields(cmdline) {
		value, ok := strings.CutPrefix(field, tunnelParam)
		if !ok {
			continue
		}
		parts := strings.Split(value, ",")
		if len(parts) != 3 {
			return tunnelSpec{}, true, fmt.Errorf("tunnel: %q must be <port>,<address>/<bits>,<gateway>", value)
		}
		spec, err := guestlisten.Parse("vsock://:" + parts[0])
		if err != nil {
			return tunnelSpec{}, true, err
		}
		guest, err := netip.ParsePrefix(parts[1])
		if err != nil || !guest.Addr().Is4() {
			return tunnelSpec{}, true, fmt.Errorf("tunnel: invalid guest address %q", parts[1])
		}
		gw, err := netip.ParseAddr(parts[2])
		if err != nil || !gw.Is4() {
			return tunnelSpec{}, true, fmt.Errorf("tunnel: invalid gateway %q", parts[2])
		}
		return tunnelSpec{port: spec.Port, guest: guest, gateway: gw}, true, nil
	}
	return tunnelSpec{}, false, nil
}

// resolverPath is the resolver configuration the tunnel points at the
// gateway.
const resolverPath = "/etc/resolv.conf"

// tunnelDeps holds the system operations of the tunnel setup, so that a test
// can inject failures without root, /dev/net/tun or a vsock listener.
type tunnelDeps struct {
	openTap            func(name string) (io.ReadWriteCloser, error)
	configureInterface func(name string, addr netip.Prefix, gateway netip.Addr) error
	writeResolver      func(path string, data []byte) error
	listen             func(address string) (net.Listener, error)
}

// systemTunnelDeps returns the operations that act on the real system.
func systemTunnelDeps() tunnelDeps {
	return tunnelDeps{
		openTap: func(name string) (io.ReadWriteCloser, error) {
			f, err := openTap(name)
			if err != nil {
				// Return an untyped nil: a nil *os.File in the interface
				// would not compare equal to nil.
				return nil, err
			}
			return f, nil
		},
		configureInterface: configureInterface,
		writeResolver: func(path string, data []byte) error {
			return os.WriteFile(path, data, 0o644)
		},
		listen: guestlisten.Listen,
	}
}

// startTunnel starts the network tunnel when the kernel command line asks
// for it. It returns after the interface is configured; the relay runs
// until the process ends. The returned channel is nil when no tunnel runs.
// Otherwise it receives the terminal error of the relay (the TAP device or
// the listener failed) after the tunnel closed its connections, listener
// and TAP device; the caller must then end the helper so that the init
// process restarts it and the host reconnects. A setup failure, including a
// failed write of the resolver configuration, is returned as an error and
// leaves no tunnel running.
func startTunnel(logger *slog.Logger) (<-chan error, error) {
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return nil, nil
	}
	spec, ok, err := parseTunnelSpec(string(cmdline))
	if !ok || err != nil {
		return nil, err
	}
	return startTunnelWith(spec, systemTunnelDeps(), logger)
}

// startTunnelWith sets the tunnel of spec up with the given operations. Every
// setup failure closes the TAP device and returns an error that names the
// failed step; the listener opens only after the interface and the resolver
// configuration are in place.
func startTunnelWith(spec tunnelSpec, deps tunnelDeps, logger *slog.Logger) (<-chan error, error) {
	tapFile, err := deps.openTap(tunnelInterface)
	if err != nil {
		return nil, fmt.Errorf("tunnel: create %s: %w", tunnelInterface, err)
	}
	if err := deps.configureInterface(tunnelInterface, spec.guest, spec.gateway); err != nil {
		tapFile.Close()
		return nil, fmt.Errorf("tunnel: configure %s: %w", tunnelInterface, err)
	}
	if err := deps.writeResolver(resolverPath, []byte("nameserver "+spec.gateway.String()+"\n")); err != nil {
		tapFile.Close()
		return nil, fmt.Errorf("tunnel: write resolver %s: %w", resolverPath, err)
	}
	l, err := deps.listen(fmt.Sprintf("vsock://:%d", spec.port))
	if err != nil {
		tapFile.Close()
		return nil, fmt.Errorf("tunnel: listen: %w", err)
	}
	t := newTunnel(tapFile, l, logger)
	t.start()
	logger.Info("network tunnel started", "port", spec.port)
	return t.fatal, nil
}

// tunnel relays frames between the TAP device and the current agent
// connection; a new connection replaces the previous one.
type tunnel struct {
	tap      io.ReadWriteCloser
	listener net.Listener
	logger   *slog.Logger
	// fatal receives the first terminal error of a worker (capacity 1, so
	// reporting never blocks). Losing only the current agent connection
	// is not terminal: a new connection replaces it.
	fatal      chan error
	acceptDone chan struct{}
	mu         sync.Mutex // guards conn and stopped
	conn       net.Conn
	stopped    bool
	wmu        sync.Mutex
}

func newTunnel(tap io.ReadWriteCloser, l net.Listener, logger *slog.Logger) *tunnel {
	return &tunnel{
		tap:        tap,
		listener:   l,
		logger:     logger,
		fatal:      make(chan error, 1),
		acceptDone: make(chan struct{}),
	}
}

// start runs the TAP reader and the accept loop.
func (t *tunnel) start() {
	go t.readTap()
	go func() {
		if err := t.accept(); err != nil {
			t.fail(err)
		}
	}()
}

// fail records the first terminal error of a worker and releases every
// resource of the tunnel, which also makes the other worker fail and
// disconnects the agent so that its reconnect loop sees the loss at once.
// Later calls change nothing.
func (t *tunnel) fail(err error) {
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	c := t.conn
	t.conn = nil
	t.mu.Unlock()

	t.listener.Close()
	t.tap.Close()
	if c != nil {
		c.Close()
	}
	// Closing the listener unblocks Accept, but a connection it already
	// accepted must be closed by the accept loop before we report failure.
	<-t.acceptDone
	t.fatal <- err
}

func (t *tunnel) accept() error {
	defer close(t.acceptDone)
	for {
		c, err := t.listener.Accept()
		if err != nil {
			t.logger.Error("network tunnel accept failed", "error", err)
			return fmt.Errorf("tunnel: accept: %w", err)
		}
		t.mu.Lock()
		if t.stopped {
			t.mu.Unlock()
			c.Close()
			return nil
		}
		if t.conn != nil {
			t.conn.Close()
		}
		t.conn = c
		go t.readConn(c)
		t.mu.Unlock()
	}
}

// closeCurrent closes c if the tunnel still owns it.
func (t *tunnel) closeCurrent(c net.Conn) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.conn == c {
		c.Close()
		t.conn = nil
	}
}

// readConn writes the frames of one agent connection to the TAP device.
func (t *tunnel) readConn(c net.Conn) {
	defer t.closeCurrent(c)
	var hdr [4]byte
	buf := make([]byte, maxFrame)
	for {
		if _, err := io.ReadFull(c, hdr[:]); err != nil {
			return
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 || n > maxFrame {
			t.logger.Warn("network tunnel frame size out of range", "size", n)
			return
		}
		if _, err := io.ReadFull(c, buf[:n]); err != nil {
			return
		}
		if _, err := t.tap.Write(buf[:n]); err != nil && !errors.Is(err, unix.EIO) {
			t.logger.Warn("network tunnel TAP write failed", "error", err)
		}
	}
}

// readTap sends the frames of the TAP device to the current connection,
// and drops them while no agent is connected.
func (t *tunnel) readTap() {
	buf := make([]byte, 4+maxFrame)
	for {
		n, err := t.tap.Read(buf[4:])
		if err != nil {
			t.logger.Error("network tunnel TAP read failed", "error", err)
			t.fail(fmt.Errorf("tunnel: TAP read: %w", err))
			return
		}
		binary.BigEndian.PutUint32(buf[:4], uint32(n))
		t.mu.Lock()
		c := t.conn
		t.mu.Unlock()
		if c == nil {
			continue
		}
		t.wmu.Lock()
		_, err = c.Write(buf[:4+n])
		t.wmu.Unlock()
		if err != nil {
			t.closeCurrent(c)
		}
	}
}

// openTap creates the TAP device name (without packet information).
func openTap(name string) (*os.File, error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		unix.Close(fd)
		return nil, err
	}
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	if err := unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return os.NewFile(uintptr(fd), "/dev/net/tun"), nil
}

// configureInterface sets the address and netmask of name, brings it up and
// adds the default route through gateway.
func configureInterface(name string, addr netip.Prefix, gateway netip.Addr) error {
	sock, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(sock)
	ifr, err := unix.NewIfreq(name)
	if err != nil {
		return err
	}
	a := addr.Addr().As4()
	if err := ifr.SetInet4Addr(a[:]); err != nil {
		return err
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCSIFADDR, ifr); err != nil {
		return fmt.Errorf("set address: %w", err)
	}
	mask := net.CIDRMask(addr.Bits(), 32)
	if err := ifr.SetInet4Addr(mask); err != nil {
		return err
	}
	if err := unix.IoctlIfreq(sock, unix.SIOCSIFNETMASK, ifr); err != nil {
		return fmt.Errorf("set netmask: %w", err)
	}
	if err := setFlagsUp(sock, name); err != nil {
		return fmt.Errorf("set up: %w", err)
	}
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return err
	}
	return addDefaultRoute(iface.Index, gateway)
}

// addDefaultRoute adds the IPv4 default route through gateway on the
// interface with index oif, with one rtnetlink RTM_NEWROUTE request.
func addDefaultRoute(oif int, gateway netip.Addr) error {
	fd, err := unix.Socket(unix.AF_NETLINK, unix.SOCK_RAW|unix.SOCK_CLOEXEC, unix.NETLINK_ROUTE)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := unix.Bind(fd, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	attr := func(typ uint16, data []byte) []byte {
		l := unix.SizeofRtAttr + len(data)
		b := make([]byte, (l+unix.RTA_ALIGNTO-1) & ^(unix.RTA_ALIGNTO-1))
		*(*unix.RtAttr)(unsafe.Pointer(&b[0])) = unix.RtAttr{Len: uint16(l), Type: typ}
		copy(b[unix.SizeofRtAttr:], data)
		return b
	}
	gw := gateway.As4()
	var idx [4]byte
	binary.NativeEndian.PutUint32(idx[:], uint32(oif))
	rtm := unix.RtMsg{Family: unix.AF_INET, Table: unix.RT_TABLE_MAIN, Protocol: unix.RTPROT_BOOT,
		Scope: unix.RT_SCOPE_UNIVERSE, Type: unix.RTN_UNICAST}
	body := (*[unix.SizeofRtMsg]byte)(unsafe.Pointer(&rtm))[:]
	payload := append(append(append([]byte{}, body...), attr(unix.RTA_GATEWAY, gw[:])...), attr(unix.RTA_OIF, idx[:])...)
	msg := make([]byte, unix.SizeofNlMsghdr+len(payload))
	*(*unix.NlMsghdr)(unsafe.Pointer(&msg[0])) = unix.NlMsghdr{
		Len:   uint32(len(msg)),
		Type:  unix.RTM_NEWROUTE,
		Flags: unix.NLM_F_REQUEST | unix.NLM_F_ACK | unix.NLM_F_CREATE | unix.NLM_F_EXCL,
		Seq:   1,
	}
	copy(msg[unix.SizeofNlMsghdr:], payload)
	if err := unix.Sendto(fd, msg, 0, &unix.SockaddrNetlink{Family: unix.AF_NETLINK}); err != nil {
		return err
	}
	resp := make([]byte, 4096)
	n, _, err := unix.Recvfrom(fd, resp, 0)
	if err != nil {
		return err
	}
	if n < unix.SizeofNlMsghdr+4 {
		return errors.New("short netlink reply")
	}
	hdr := (*unix.NlMsghdr)(unsafe.Pointer(&resp[0]))
	if hdr.Type == unix.NLMSG_ERROR {
		if code := int32(binary.NativeEndian.Uint32(resp[unix.SizeofNlMsghdr:])); code != 0 {
			return fmt.Errorf("add default route: %w", unix.Errno(-code))
		}
	}
	return nil
}
