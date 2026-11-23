// SPDX-License-Identifier: FSL-1.1-ALv2

package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
)

// guest is a second gVisor stack that plays the sandbox guest: it sends
// its Ethernet frames over a stream with the length-prefixed framing of the
// guest helper's network tunnel.
type guest struct {
	stack *stack.Stack
	ch    *channel.Endpoint
}

func newGuest(t *testing.T, link netpolicy.Link, mac net.HardwareAddr, conn net.Conn) *guest {
	t.Helper()
	ch := channel.New(256, MTU, tcpip.LinkAddress(mac))
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := s.CreateNIC(1, ethernet.New(ch)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(link.Guest.As4()), PrefixLen: link.Bits},
	}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(link.Gateway.As4()), NIC: 1}})
	g := &guest{stack: s, ch: ch}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		conn.Close()
		s.Close()
	})
	// Outbound frames: guest stack to the stream.
	go func() {
		for {
			pkt := ch.ReadContext(ctx)
			if pkt == nil {
				return
			}
			frame := pkt.ToView().AsSlice()
			pkt.DecRef()
			var hdr [4]byte
			binary.BigEndian.PutUint32(hdr[:], uint32(len(frame)))
			if _, err := conn.Write(append(hdr[:], frame...)); err != nil {
				return
			}
		}
	}()
	// Inbound frames: the stream to the guest stack.
	go func() {
		var hdr [4]byte
		for {
			if _, err := io.ReadFull(conn, hdr[:]); err != nil {
				return
			}
			frame := make([]byte, binary.BigEndian.Uint32(hdr[:]))
			if _, err := io.ReadFull(conn, frame); err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
			ch.InjectInbound(0, pkt)
			pkt.DecRef()
		}
	}()
	return g
}

func (g *guest) dial(ctx context.Context, addr string) (net.Conn, error) {
	ap := netip.MustParseAddrPort(addr)
	return gonet.DialContextTCP(ctx, g.stack, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
}

// TestNetworkForwardsAllowedAndBlocksPolicyDestinations runs a guest stack
// against a sandbox network over the stream framing: a TCP connection to
// an allowed public address reaches the host dialer, and connections to
// private, loopback and host addresses are reset without a host dial.
func TestNetworkForwardsAllowedAndBlocksPolicyDestinations(t *testing.T) {
	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	filter := netpolicy.NewFilter(logger)
	filter.SetRoutesForTest(func() ([]netpolicy.Route, []netip.Prefix, error) {
		return []netpolicy.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"}},
			[]netip.Prefix{netip.MustParsePrefix("198.51.100.7/24")}, nil
	})
	if err := filter.Refresh(); err != nil {
		t.Fatal(err)
	}
	link, _ := netpolicy.LinkFor(1)
	mac, _ := RandomMAC()
	dials := make(chan string, 8)
	n, err := New(Options{SandboxID: "test", Link: link, GuestMAC: mac, Filter: filter, Logger: logger,
		dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			dials <- address
			if address == "203.0.113.10:80" {
				var d net.Dialer
				return d.DialContext(ctx, network, echo.Addr().String())
			}
			return nil, errors.New("unexpected dial")
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	agentSide, guestSide := net.Pipe()
	go n.Serve(context.Background(), agentSide, FramingLengthPrefixed)
	g := newGuest(t, link, mac, guestSide)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("dial allowed destination: %v", err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
	c.Close()
	if got := <-dials; got != "203.0.113.10:80" {
		t.Fatalf("host dial %q", got)
	}

	for _, blocked := range []string{"192.168.1.1:80", "127.0.0.1:80", "10.1.2.3:443", "198.51.100.7:22", "198.51.100.99:80", "169.254.169.254:80"} {
		if c, err := g.dial(ctx, blocked); err == nil {
			c.Close()
			t.Fatalf("guest connected to blocked destination %s", blocked)
		}
	}
	select {
	case got := <-dials:
		t.Fatalf("host dialed blocked destination %s", got)
	default:
	}
}

type closeRecorder struct{ closed chan struct{} }

func (c *closeRecorder) Close() error {
	select {
	case <-c.closed:
	default:
		close(c.closed)
	}
	return nil
}

// A flow that starts after Close gets its connections closed and no relay,
// and Close returns while a forwarded host connection stays open.
func TestNetworkCloseEndsEveryFlow(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	filter := netpolicy.NewFilter(logger)
	filter.SetRoutesForTest(func() ([]netpolicy.Route, []netip.Prefix, error) {
		return []netpolicy.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"}}, nil, nil
	})
	if err := filter.Refresh(); err != nil {
		t.Fatal(err)
	}
	// The host side accepts and never writes or closes.
	silent, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer silent.Close()
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	link, _ := netpolicy.LinkFor(1)
	mac, _ := RandomMAC()
	n, err := New(Options{SandboxID: "test", Link: link, GuestMAC: mac, Filter: filter, Logger: logger,
		dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, silent.Addr().String())
		}})
	if err != nil {
		t.Fatal(err)
	}
	agentSide, guestSide := net.Pipe()
	go n.Serve(context.Background(), agentSide, FramingLengthPrefixed)
	g := newGuest(t, link, mac, guestSide)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	done := make(chan struct{})
	go func() { n.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while a forwarded connection was open")
	}

	a, b := &closeRecorder{closed: make(chan struct{})}, &closeRecorder{closed: make(chan struct{})}
	ran := false
	if n.startProxy(func() { ran = true }, a, b) {
		t.Fatal("startProxy started a flow on a closed network")
	}
	for _, r := range []*closeRecorder{a, b} {
		select {
		case <-r.closed:
		default:
			t.Fatal("startProxy left a connection of a refused flow open")
		}
	}
	if ran {
		t.Fatal("startProxy ran the relay of a refused flow")
	}
}

// peerUDPConn is a loopback UDP socket that sends every datagram to one fixed
// peer address. It stands for a connected socket without calling connect, so
// it needs no port to be released and re-bound, and it works the same on every
// platform. Reads, deadlines and Close come from the embedded socket.
type peerUDPConn struct {
	*net.UDPConn
	peer *net.UDPAddr
}

func (c *peerUDPConn) Write(b []byte) (int, error) { return c.WriteToUDP(b, c.peer) }

func (c *peerUDPConn) RemoteAddr() net.Addr { return c.peer }

// udpPair returns two loopback UDP sockets that send to each other. Both
// sockets stay bound to the port the system assigned them, and a cleanup
// registered right after each bind closes them, also when the setup fails.
func udpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	bind := func() *net.UDPConn {
		t.Helper()
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { c.Close() })
		return c
	}
	a, b := bind(), bind()
	return &peerUDPConn{UDPConn: a, peer: b.LocalAddr().(*net.UDPAddr)},
		&peerUDPConn{UDPConn: b, peer: a.LocalAddr().(*net.UDPAddr)}
}

// A UDP flow whose datagrams go one way stays open while that direction is
// active, and ends after the idle time without traffic in either direction.
func TestProxyUDPIdleCountsBothDirections(t *testing.T) {
	guest, in := udpPair(t)
	out, host := udpPair(t)
	const idle = 300 * time.Millisecond
	ended := make(chan struct{})
	go func() { proxyUDP(in, out, idle); close(ended) }()

	received := make(chan struct{}, 64)
	go func() {
		buf := make([]byte, 16)
		for {
			if _, err := host.Read(buf); err != nil {
				return
			}
			received <- struct{}{}
		}
	}()
	// Send guest-to-host only, for about three idle periods.
	for i := 0; i < 15; i++ {
		if _, err := guest.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-received:
		case <-time.After(time.Second):
			t.Fatalf("datagram %d was not relayed: the flow ended while it was active", i)
		}
		time.Sleep(60 * time.Millisecond)
	}
	select {
	case <-ended:
		t.Fatal("the flow ended while one direction was active")
	default:
	}
	select {
	case <-ended:
	case <-time.After(5 * idle):
		t.Fatal("the flow did not end after the idle time")
	}
}

// fakeElapsed is an elapsed-time source that a test advances by hand. It
// stands for the monotonic clock: a wall-clock step does not change it.
type fakeElapsed struct{ now atomic.Int64 }

func (f *fakeElapsed) elapsed() time.Duration  { return time.Duration(f.now.Load()) }
func (f *fakeElapsed) advance(d time.Duration) { f.now.Add(int64(d)) }

// The idle time of a flow counts elapsed time since the latest datagram in
// either direction and ends after the configured idle time without traffic.
func TestIdleTimerExpiresAfterIdleWithoutTraffic(t *testing.T) {
	const idle = 90 * time.Second
	clock := &fakeElapsed{}
	timer := newIdleTimer(idle, clock.elapsed)
	clock.advance(idle - time.Nanosecond)
	if timer.remaining() <= 0 {
		t.Fatal("the flow ended before the idle time passed")
	}
	clock.advance(time.Nanosecond)
	if timer.remaining() > 0 {
		t.Fatal("the flow stayed open after the idle time passed")
	}
}

// Every datagram moves the shared deadline; a datagram from either goroutine
// uses the same timer.
func TestIdleTimerTouchMovesDeadline(t *testing.T) {
	const idle = 90 * time.Second
	clock := &fakeElapsed{}
	timer := newIdleTimer(idle, clock.elapsed)
	for i := 0; i < 5; i++ {
		clock.advance(idle - time.Second)
		timer.touch()
		if got := timer.remaining(); got != idle {
			t.Fatalf("remaining after a datagram = %v, want %v", got, idle)
		}
	}
	clock.advance(idle)
	if timer.remaining() > 0 {
		t.Fatal("the flow stayed open after the idle time without traffic")
	}
}

// The flow idle time follows only the injected elapsed source, which stands
// for the monotonic clock: a wall-clock step, forward or backward, changes
// neither the elapsed time nor the idle lifetime. Real time passes here while
// the fake elapsed time stays still, so the flow must stay open; a datagram
// after a partial advance moves the deadline for both directions; and the flow
// ends once the elapsed time passes the idle time without traffic.
func TestProxyUDPIdleFollowsElapsedSource(t *testing.T) {
	guest, in := udpPair(t)
	out, host := udpPair(t)
	const idle = 200 * time.Millisecond
	clock := &fakeElapsed{}
	ended := make(chan struct{})
	go func() { proxyUDPWithClock(in, out, idle, clock.elapsed); close(ended) }()

	relayed := func(src, dst net.Conn, what string) {
		t.Helper()
		if _, err := src.Write([]byte("ping")); err != nil {
			t.Fatal(err)
		}
		_ = dst.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 16)
		if _, err := dst.Read(buf); err != nil {
			t.Fatalf("%s datagram was not relayed: %v", what, err)
		}
	}
	// Real time passes for several idle periods with a still elapsed clock.
	time.Sleep(3 * idle)
	select {
	case <-ended:
		t.Fatal("the flow ended although no elapsed time passed")
	default:
	}
	// Both directions refresh the shared deadline.
	clock.advance(idle - time.Millisecond)
	relayed(guest, host, "guest-to-host")
	clock.advance(idle - time.Millisecond)
	relayed(host, guest, "host-to-guest")
	time.Sleep(3 * idle)
	select {
	case <-ended:
		t.Fatal("the flow ended although each datagram refreshed the deadline")
	default:
	}
	// Without traffic, the passing elapsed time closes the flow.
	clock.advance(idle)
	select {
	case <-ended:
	case <-time.After(5 * idle):
		t.Fatal("the flow did not end after the idle time without traffic")
	}
}

// The monotonic elapsed source ignores wall-clock steps, because the start
// time keeps its monotonic reading and time.Since uses it.
func TestMonotonicElapsedUsesMonotonicReading(t *testing.T) {
	elapsed := newMonotonicElapsed()
	first := elapsed()
	time.Sleep(5 * time.Millisecond)
	second := elapsed()
	if second <= first || second < 5*time.Millisecond {
		t.Fatalf("elapsed went from %v to %v after 5ms", first, second)
	}
}

// An out-of-order touch from a slower goroutine does not move the recorded
// datagram time backward.
func TestIdleTimerTouchNeverMovesBackward(t *testing.T) {
	clock := &fakeElapsed{}
	timer := newIdleTimer(time.Minute, clock.elapsed)
	clock.advance(10 * time.Second)
	timer.touch()
	clock.now.Store(int64(5 * time.Second))
	timer.touch()
	if got := time.Duration(timer.last.Load()); got != 10*time.Second {
		t.Fatalf("last = %v, want 10s", got)
	}
}

// allowAllFilter returns a filter that allows every public destination (the
// private, loopback and host ranges stay blocked, as in production).
func allowAllFilter(t *testing.T, logger *slog.Logger) *netpolicy.Filter {
	t.Helper()
	filter := netpolicy.NewFilter(logger)
	filter.SetRoutesForTest(func() ([]netpolicy.Route, []netip.Prefix, error) {
		return []netpolicy.Route{{Prefix: netip.MustParsePrefix("0.0.0.0/0"), Interface: "en0"}}, nil, nil
	})
	if err := filter.Refresh(); err != nil {
		t.Fatal(err)
	}
	return filter
}

// newLimitedNetwork starts a sandbox network with the given limits and dial
// and attaches a guest stack to it. The network closes with the test.
func newLimitedNetwork(t *testing.T, limits flowLimits, logger *slog.Logger,
	dial func(ctx context.Context, network, address string) (net.Conn, error)) (*Network, *guest) {
	t.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	link, _ := netpolicy.LinkFor(1)
	mac, _ := RandomMAC()
	n, err := New(Options{SandboxID: "test", Link: link, GuestMAC: mac, Filter: allowAllFilter(t, logger),
		Logger: logger, dial: dial, limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(n.Close)
	agentSide, guestSide := net.Pipe()
	go n.Serve(context.Background(), agentSide, FramingLengthPrefixed)
	return n, newGuest(t, link, mac, guestSide)
}

// silentTCPServer accepts connections and never writes. Every accepted
// connection is sent on the returned channel.
func silentTCPServer(t *testing.T) (*net.TCPListener, <-chan net.Conn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	accepted := make(chan net.Conn, 16)
	go func() {
		for {
			c, err := ln.AcceptTCP()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		for {
			select {
			case c := <-accepted:
				c.Close()
			default:
				return
			}
		}
	})
	return ln, accepted
}

// udpEchoSink echoes every datagram to its sender.
func udpEchoSink(t *testing.T) *net.UDPConn {
	t.Helper()
	sink, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sink.Close() })
	go func() {
		buf := make([]byte, 64)
		for {
			k, from, err := sink.ReadFromUDP(buf)
			if err != nil {
				return
			}
			_, _ = sink.WriteToUDP(buf[:k], from)
		}
	}()
	return sink
}

// routeDial dials the silent TCP server for TCP and the echo sink for UDP and
// counts the host dials of each network.
type routeDial struct {
	tcpAddr, udpAddr string
	tcp, udp         atomic.Int64
	failTCP          atomic.Int64
}

func (d *routeDial) dial(ctx context.Context, network, _ string) (net.Conn, error) {
	var nd net.Dialer
	switch network {
	case "tcp":
		d.tcp.Add(1)
		if d.failTCP.Add(-1) >= 0 {
			return nil, errors.New("dial refused by the test")
		}
		return nd.DialContext(ctx, network, d.tcpAddr)
	case "udp":
		d.udp.Add(1)
		return nd.DialContext(ctx, network, d.udpAddr)
	}
	return nil, errors.New("unexpected network " + network)
}

// warmUp makes the guest resolve the ARP entry of the gateway with a TCP
// connection to a blocked destination, which takes no slot and no dial.
func warmUp(t *testing.T, g *guest) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if c, err := g.dial(ctx, "192.168.1.1:80"); err == nil {
		c.Close()
		t.Fatal("guest connected to a blocked destination")
	}
}

func (g *guest) dialUDP(t *testing.T, addr string) net.Conn {
	t.Helper()
	ap := netip.MustParseAddrPort(addr)
	c, err := gonet.DialUDP(g.stack, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ap.Addr().As4()), Port: ap.Port()}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

// echoed sends one datagram and reports whether the echo arrives within wait.
func echoed(t *testing.T, c net.Conn, wait time.Duration) bool {
	t.Helper()
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 16)
	_, err := c.Read(buf)
	return err == nil
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// With the TCP limit at 2, the third guest connection is reset and makes no
// host dial; after one of the first two ends, a new connection is admitted.
func TestTCPFlowLimit(t *testing.T) {
	srv, accepted := silentTCPServer(t)
	rd := &routeDial{tcpAddr: srv.Addr().String()}
	n, g := newLimitedNetwork(t, flowLimits{maxTCPConns: 2, tcpHalfCloseIdle: 200 * time.Millisecond}, nil, rd.dial)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	a, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("first connection: %v", err)
	}
	b, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	defer b.Close()
	hostA := <-accepted

	cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
	if c, err := g.dial(cctx, "203.0.113.10:80"); err == nil {
		c.Close()
		t.Fatal("the connection beyond the limit was admitted")
	}
	ccancel()
	if got := rd.tcp.Load(); got != 2 {
		t.Fatalf("host dials = %d after the rejected connection, want 2", got)
	}
	if got := n.limiter.inUse(TransportTCP); got != 2 {
		t.Fatalf("TCP slots in use = %d, want 2", got)
	}

	// The guest closes A; the host side sees the end, and once the silent
	// remote side idles out, the slot returns.
	a.Close()
	_ = hostA.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := hostA.Read(make([]byte, 1)); err == nil {
		t.Fatal("the host socket of the closed guest connection stayed open for reads")
	}
	waitFor(t, "the TCP slot of the closed connection", func() bool { return n.limiter.inUse(TransportTCP) == 1 })
	d, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("connection after a slot returned: %v", err)
	}
	d.Close()
	if got := rd.tcp.Load(); got != 3 {
		t.Fatalf("host dials = %d, want 3", got)
	}
}

// With the UDP limit at 2, datagrams of a third source port make no host dial
// and the first two flows keep relaying; after a flow ends by idle time, a new
// source port is admitted.
func TestUDPFlowLimit(t *testing.T) {
	sink := udpEchoSink(t)
	rd := &routeDial{udpAddr: sink.LocalAddr().String()}
	n, g := newLimitedNetwork(t, flowLimits{maxUDPFlows: 2, udpIdle: 600 * time.Millisecond}, nil, rd.dial)
	warmUp(t, g)

	c1 := g.dialUDP(t, "203.0.113.10:9000")
	c2 := g.dialUDP(t, "203.0.113.10:9000")
	c3 := g.dialUDP(t, "203.0.113.10:9000")
	if !echoed(t, c1, 2*time.Second) || !echoed(t, c2, 2*time.Second) {
		t.Fatal("an admitted UDP flow got no echo")
	}
	if echoed(t, c3, 500*time.Millisecond) {
		t.Fatal("the flow beyond the limit was relayed")
	}
	if got := rd.udp.Load(); got != 2 {
		t.Fatalf("host dials = %d, want 2", got)
	}
	// The first two flows keep relaying in both directions.
	if !echoed(t, c1, 2*time.Second) || !echoed(t, c2, 2*time.Second) {
		t.Fatal("an admitted UDP flow stopped relaying after a rejection")
	}
	waitFor(t, "the UDP slots to return", func() bool { return n.limiter.inUse(TransportUDP) == 0 })
	c4 := g.dialUDP(t, "203.0.113.10:9000")
	if !echoed(t, c4, 2*time.Second) {
		t.Fatal("a new UDP flow was not admitted after the idle end")
	}
	if got := rd.udp.Load(); got != 3 {
		t.Fatalf("host dials = %d, want 3", got)
	}
}

// A full TCP table does not block UDP and a full UDP table does not block TCP;
// a second network has its own limits.
func TestFlowLimitsAreSeparatePerTransportAndPerNetwork(t *testing.T) {
	srv, _ := silentTCPServer(t)
	sink := udpEchoSink(t)
	rd := &routeDial{tcpAddr: srv.Addr().String(), udpAddr: sink.LocalAddr().String()}
	limits := flowLimits{maxTCPConns: 1, maxUDPFlows: 1, udpIdle: time.Minute, tcpHalfCloseIdle: 200 * time.Millisecond}
	n, g := newLimitedNetwork(t, limits, nil, rd.dial)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	warmUp(t, g)

	// Fill the TCP table; a UDP flow still dials.
	tcpConn, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatal(err)
	}
	udpConn := g.dialUDP(t, "203.0.113.10:9000")
	if !echoed(t, udpConn, 2*time.Second) {
		t.Fatal("a full TCP table blocked a new UDP flow")
	}
	// The UDP table is full now. Free the TCP slot; a new TCP connection is
	// admitted although the UDP table stays full.
	tcpConn.Close()
	waitFor(t, "the TCP slot", func() bool { return n.limiter.inUse(TransportTCP) == 0 })
	if got := n.limiter.inUse(TransportUDP); got != 1 {
		t.Fatalf("UDP slots in use = %d, want 1", got)
	}
	tcp2, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("a full UDP table blocked a new TCP connection: %v", err)
	}
	defer tcp2.Close()

	// Both tables of the first network are full; a second network is not
	// affected.
	rd2 := &routeDial{tcpAddr: srv.Addr().String(), udpAddr: sink.LocalAddr().String()}
	_, g2 := newLimitedNetwork(t, limits, nil, rd2.dial)
	tcp3, err := g2.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("a full network blocked a connection of another network: %v", err)
	}
	defer tcp3.Close()
	if !echoed(t, g2.dialUDP(t, "203.0.113.10:9000"), 2*time.Second) {
		t.Fatal("a full network blocked a UDP flow of another network")
	}
}

// capturedRecord is one log record with the attributes of its handler chain.
type capturedRecord struct {
	level   slog.Level
	message string
	attrs   map[string]any
}

type captureHandler struct {
	mu      *sync.Mutex
	records *[]capturedRecord
	attrs   []slog.Attr
}

func newCaptureLogger() (*slog.Logger, func() []capturedRecord) {
	h := &captureHandler{mu: &sync.Mutex{}, records: &[]capturedRecord{}}
	return slog.New(h), func() []capturedRecord {
		h.mu.Lock()
		defer h.mu.Unlock()
		return append([]capturedRecord(nil), *h.records...)
	}
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, message: r.Message, attrs: map[string]any{}}
	for _, a := range h.attrs {
		rec.attrs[a.Key] = a.Value.Any()
	}
	r.Attrs(func(a slog.Attr) bool { rec.attrs[a.Key] = a.Value.Any(); return true })
	h.mu.Lock()
	*h.records = append(*h.records, rec)
	h.mu.Unlock()
	return nil
}

func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &captureHandler{mu: h.mu, records: h.records, attrs: append(append([]slog.Attr(nil), h.attrs...), attrs...)}
}

func (h *captureHandler) WithGroup(string) slog.Handler { return h }

func capacityRecords(records []capturedRecord) []capturedRecord {
	var out []capturedRecord
	for _, r := range records {
		if r.message == "sandbox network stack at capacity; rejected flows" {
			out = append(out, r)
		}
	}
	return out
}

// Blocked destinations take no slot and log no rejection.
func TestBlockedDestinationsTakeNoSlot(t *testing.T) {
	srv, _ := silentTCPServer(t)
	rd := &routeDial{tcpAddr: srv.Addr().String()}
	logger, records := newCaptureLogger()
	n, g := newLimitedNetwork(t, flowLimits{maxTCPConns: 1}, logger, rd.dial)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for i := 0; i < 5; i++ {
		if c, err := g.dial(ctx, "192.168.1.1:80"); err == nil {
			c.Close()
			t.Fatal("guest connected to a blocked destination")
		}
	}
	if got := n.limiter.inUse(TransportTCP); got != 0 {
		t.Fatalf("a blocked destination took %d slots", got)
	}
	c, err := g.dial(ctx, "203.0.113.10:80")
	if err != nil {
		t.Fatalf("an allowed flow after blocked attempts: %v", err)
	}
	c.Close()
	if got := rd.tcp.Load(); got != 1 {
		t.Fatalf("host dials = %d, want 1", got)
	}
	if got := capacityRecords(records()); len(got) != 0 {
		t.Fatalf("blocked destinations logged %d rejection records", len(got))
	}
}

// A failed host dial releases its slot, and after Close no slot is in use.
func TestFlowSlotReleasedOnEveryPath(t *testing.T) {
	srv, _ := silentTCPServer(t)
	sink := udpEchoSink(t)
	rd := &routeDial{tcpAddr: srv.Addr().String(), udpAddr: sink.LocalAddr().String()}
	rd.failTCP.Store(1)
	n, g := newLimitedNetwork(t, flowLimits{maxTCPConns: 1}, nil, rd.dial)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	warmUp(t, g)

	// The first host dial fails and the guest gets a reset.
	if c, err := g.dial(ctx, "203.0.113.10:80"); err == nil {
		c.Close()
		t.Fatal("the guest connected although the host dial failed")
	}
	waitFor(t, "the slot of the failed dial", func() bool { return n.limiter.inUse(TransportTCP) == 0 })
	// With the limit at 1, a leaked slot would reject this connection.
	if _, err := g.dial(ctx, "203.0.113.10:80"); err != nil {
		t.Fatalf("connection after a failed dial: %v", err)
	}
	if !echoed(t, g.dialUDP(t, "203.0.113.10:9000"), 2*time.Second) {
		t.Fatal("UDP flow was not relayed")
	}
	if tcpUse, udpUse := n.limiter.inUse(TransportTCP), n.limiter.inUse(TransportUDP); tcpUse != 1 || udpUse != 1 {
		t.Fatalf("slots in use before Close: tcp=%d udp=%d, want 1 and 1", tcpUse, udpUse)
	}
	n.Close()
	if tcpUse, udpUse := n.limiter.inUse(TransportTCP), n.limiter.inUse(TransportUDP); tcpUse != 0 || udpUse != 0 {
		t.Fatalf("slots in use after Close: tcp=%d udp=%d, want 0", tcpUse, udpUse)
	}
}

// A slot returns once however often release is called.
func TestFlowSlotReleaseIsIdempotent(t *testing.T) {
	logger, _ := newCaptureLogger()
	l := newFlowLimiter(logger, flowLimits{maxTCPConns: 1, maxUDPFlows: 1}, time.Now)
	release, ok := l.acquire(TransportTCP)
	if !ok {
		t.Fatal("first acquire failed")
	}
	release()
	release()
	if got := l.inUse(TransportTCP); got != 0 {
		t.Fatalf("slots in use = %d after releases, want 0", got)
	}
	first, _ := l.acquire(TransportTCP)
	if _, ok := l.acquire(TransportTCP); ok {
		t.Fatal("a double release freed a slot that another flow holds")
	}
	first()
}

// tcpLoopbackPair returns the two ends of one loopback TCP connection.
func tcpLoopbackPair(t *testing.T) (client, server *net.TCPConn) {
	t.Helper()
	ln, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan *net.TCPConn, 1)
	go func() {
		c, _ := ln.AcceptTCP()
		accepted <- c
	}()
	client, err = net.DialTCP("tcp", nil, ln.Addr().(*net.TCPAddr))
	if err != nil {
		t.Fatal(err)
	}
	server = <-accepted
	t.Cleanup(func() { client.Close(); server.Close() })
	return client, server
}

// After one direction ended, proxy ends the flow once the other direction is
// idle for the half-close time, and any data in that direction renews the time.
func TestProxyHalfCloseIdleEndsFlow(t *testing.T) {
	const idle = 300 * time.Millisecond
	t.Run("silent remote side", func(t *testing.T) {
		c1, s1 := tcpLoopbackPair(t)
		c2, s2 := tcpLoopbackPair(t)
		ended := make(chan struct{})
		start := time.Now()
		go func() { proxy(s1, s2, idle); close(ended) }()
		// The guest side ends its direction; the remote side stays silent.
		if err := c1.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		select {
		case <-ended:
		case <-time.After(idle + time.Second):
			t.Fatal("proxy did not end after the half-close idle time")
		}
		if took := time.Since(start); took < idle/2 {
			t.Fatalf("proxy ended after %v, before the half-close idle time", took)
		}
		// Both outer peers see the end of the flow.
		for name, c := range map[string]*net.TCPConn{"guest side": c1, "remote side": c2} {
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("%s read after the flow ended = %v, want EOF or a connection error", name, err)
			}
		}
	})
	t.Run("data renews the time", func(t *testing.T) {
		c1, s1 := tcpLoopbackPair(t)
		c2, s2 := tcpLoopbackPair(t)
		ended := make(chan struct{})
		go func() { proxy(s1, s2, idle); close(ended) }()
		if err := c1.CloseWrite(); err != nil {
			t.Fatal(err)
		}
		go io.Copy(io.Discard, c1)
		// The remote side keeps sending for three idle periods.
		for deadline := time.Now().Add(3 * idle); time.Now().Before(deadline); {
			if _, err := c2.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
			select {
			case <-ended:
				t.Fatal("proxy ended while the other direction carried data")
			case <-time.After(idle / 3):
			}
		}
		select {
		case <-ended:
		case <-time.After(idle + time.Second):
			t.Fatal("proxy did not end after the data stopped")
		}
	})
}

// fakeClock is a wall clock that a test advances by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// Rejections log at most once per interval and transport, the first at once,
// with the count since the previous record and no other attributes than the
// sandbox ID of the logger.
func TestFlowRejectLogRateLimit(t *testing.T) {
	logger, records := newCaptureLogger()
	clock := &fakeClock{now: time.Unix(1000, 0)}
	l := newFlowLimiter(logger.With("sandbox_id", "sb"), flowLimits{maxTCPConns: 1, maxUDPFlows: 2}, clock.Now)
	if _, ok := l.acquire(TransportTCP); !ok {
		t.Fatal("first TCP acquire failed")
	}
	for i := 0; i < 100; i++ {
		if _, ok := l.acquire(TransportTCP); ok {
			t.Fatal("acquire beyond the limit succeeded")
		}
	}
	check := func(rec capturedRecord, transport string, rejected, limit int) {
		t.Helper()
		want := map[string]any{"sandbox_id": "sb", "transport": transport, "rejected": int64(rejected), "limit": int64(limit)}
		if rec.level != slog.LevelWarn || len(rec.attrs) != len(want) {
			t.Fatalf("record %+v, want a warning with attributes %v", rec, want)
		}
		for k, v := range want {
			if rec.attrs[k] != v {
				t.Fatalf("record %+v: attribute %s = %v, want %v", rec, k, rec.attrs[k], v)
			}
		}
	}
	got := capacityRecords(records())
	if len(got) != 1 {
		t.Fatalf("%d records after 100 rejections, want 1", len(got))
	}
	check(got[0], "tcp", 1, 1)

	// UDP counts independently and its first rejection logs at once.
	l.acquire(TransportUDP)
	l.acquire(TransportUDP)
	for i := 0; i < 3; i++ {
		l.acquire(TransportUDP)
	}
	got = capacityRecords(records())
	if len(got) != 2 {
		t.Fatalf("%d records after the UDP rejections, want 2", len(got))
	}
	check(got[1], "udp", 1, 2)

	clock.advance(flowRejectLogInterval - time.Nanosecond)
	l.acquire(TransportTCP)
	if got := capacityRecords(records()); len(got) != 2 {
		t.Fatalf("%d records before the interval passed, want 2", len(got))
	}
	clock.advance(time.Nanosecond)
	l.acquire(TransportTCP)
	got = capacityRecords(records())
	if len(got) != 3 {
		t.Fatalf("%d records after the interval, want 3", len(got))
	}
	// 99 skipped rejections, one more before the boundary and the new one.
	check(got[2], "tcp", 101, 1)
	l.acquire(TransportUDP)
	got = capacityRecords(records())
	if len(got) != 4 {
		t.Fatalf("%d records for UDP after the interval, want 4", len(got))
	}
	check(got[3], "udp", 3, 2)
}
