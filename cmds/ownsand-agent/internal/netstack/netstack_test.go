// SPDX-License-Identifier: Apache-2.0

package netstack

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
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
