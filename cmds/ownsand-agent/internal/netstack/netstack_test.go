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

// udpPair returns two connected loopback UDP sockets.
func udpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	a, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	b, err := net.DialUDP("udp", nil, a.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	// Connect a to b, so both are connected sockets.
	if _, err := b.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_, from, err := a.ReadFromUDP(buf)
	if err != nil {
		t.Fatal(err)
	}
	a.Close()
	ac, err := net.DialUDP("udp", a.LocalAddr().(*net.UDPAddr), from)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ac.Close(); b.Close() })
	return ac, b
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
