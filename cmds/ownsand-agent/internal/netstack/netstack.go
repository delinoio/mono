// SPDX-License-Identifier: FSL-1.1-ALv2

// Package netstack is the userspace network stack of the OUTBOUND network
// mode on macOS and Windows hosts. Each sandbox gets its own stack (the
// gVisor network stack, through the building blocks of gvisor-tap-vsock)
// that terminates the guest's Ethernet link inside the agent: it answers ARP
// and DHCPv4 for the guest link, serves DNS at the gateway address through
// the host resolver, and opens every TCP connection and UDP flow of the
// guest from the agent with a host socket, after the netpolicy Filter
// allowed the destination. Packets to blocked destinations never leave the
// agent, inbound connections to the guest do not exist, and the guest has
// no IPv6 address, as on Linux hosts. ICMP is not forwarded.
//
// The macOS runtime connects the stack to a Virtualization.framework
// file-handle network attachment (one Ethernet frame per datagram); the
// Windows runtime connects it to the guest helper's network tunnel over a
// Hyper-V socket (frames with a 32-bit big-endian length). See
// docs/cmds-ownsand-agent-foundation.md#network-policy.
package netstack

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/containers/gvisor-tap-vsock/pkg/services/dhcp"
	"github.com/containers/gvisor-tap-vsock/pkg/services/dns"
	"github.com/containers/gvisor-tap-vsock/pkg/tap"
	"github.com/containers/gvisor-tap-vsock/pkg/types"
	"github.com/sirupsen/logrus"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/netpolicy"
)

// Framing names how Ethernet frames travel on the connection to the guest.
type Framing int

const (
	// FramingDatagram carries one frame per datagram (a unix datagram
	// socket pair of a Virtualization.framework file-handle attachment).
	FramingDatagram Framing = iota + 1
	// FramingLengthPrefixed carries frames on a stream, each after its
	// length as a 32-bit big-endian integer (the guest helper's network
	// tunnel).
	FramingLengthPrefixed
)

func (f Framing) protocol() (types.Protocol, error) {
	switch f {
	case FramingDatagram:
		return types.VfkitProtocol, nil
	case FramingLengthPrefixed:
		return types.QemuProtocol, nil
	}
	return "", fmt.Errorf("netstack: unknown framing %d", f)
}

// MTU of the guest link.
const MTU = 1500

// nicID is the only NIC of each stack.
const nicID tcpip.NICID = 1

// Timeouts of the forwarders.
const (
	tcpConnectTimeout = 30 * time.Second
	// udpIdleTimeout ends a UDP flow without traffic in either direction.
	udpIdleTimeout = 90 * time.Second
	// tcpHalfCloseIdleTimeout ends a TCP connection whose one direction has
	// ended, once the other direction carried no data for this long. A
	// connection with both directions open has no idle timeout.
	tcpHalfCloseIdleTimeout = 30 * time.Second
	maxInFlightTCP          = 256
)

// Default admission limits of one sandbox network. Each admitted flow holds
// one host socket and goroutines of the shared agent process, and a UDP flow
// also holds two 65535-byte relay buffers, so the limits bound what one
// untrusted guest can make the agent open. The two transports have separate
// limits, so a flood of one never starves the other.
const (
	// defaultMaxTCPConns is the value of maxInFlightTCP, the repository's
	// bound on simultaneous TCP work for one sandbox.
	defaultMaxTCPConns = maxInFlightTCP
	defaultMaxUDPFlows = 256
)

// flowRejectLogInterval is the minimum time between two rejection log
// records of one transport. Rejections in between only increase the count
// that the next record reports.
const flowRejectLogInterval = 5 * time.Second

func init() {
	// gvisor-tap-vsock logs through logrus; the agent logs through slog
	// and the stack reports its own errors. Keep only logrus errors, on
	// standard error like the agent's JSON logs.
	logrus.SetLevel(logrus.ErrorLevel)
	logrus.SetFormatter(&logrus.JSONFormatter{})
}

// Options configure one sandbox network.
type Options struct {
	SandboxID string
	// Link is the guest link: the gateway address of the stack and the
	// guest address.
	Link netpolicy.Link
	// GuestMAC is the MAC address of the guest interface, for the static
	// DHCP lease.
	GuestMAC net.HardwareAddr
	// Filter decides which destinations the guest may reach.
	Filter *netpolicy.Filter
	Logger *slog.Logger
	// dial opens host connections; tests replace it.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
	// limits override the admission limits and idle times; tests set it and
	// production leaves it zero, which selects the defaults.
	limits flowLimits
}

// flowLimits holds the admission limits and idle times of one sandbox
// network. A zero field selects its default.
type flowLimits struct {
	maxUDPFlows int
	maxTCPConns int
	// udpIdle ends a UDP flow without traffic in either direction.
	udpIdle time.Duration
	// tcpHalfCloseIdle ends a TCP connection whose one direction has ended
	// once the other direction carried no data for this long.
	tcpHalfCloseIdle time.Duration
}

func (l flowLimits) withDefaults() flowLimits {
	if l.maxUDPFlows <= 0 {
		l.maxUDPFlows = defaultMaxUDPFlows
	}
	if l.maxTCPConns <= 0 {
		l.maxTCPConns = defaultMaxTCPConns
	}
	if l.udpIdle <= 0 {
		l.udpIdle = udpIdleTimeout
	}
	if l.tcpHalfCloseIdle <= 0 {
		l.tcpHalfCloseIdle = tcpHalfCloseIdleTimeout
	}
	return l
}

// Transport names a transport protocol of the guest flows that the stack
// forwards.
type Transport string

const (
	TransportTCP Transport = "tcp"
	TransportUDP Transport = "udp"
)

// flowSlots is the admission state of one transport.
type flowSlots struct {
	limit  int
	inUse  int
	logged bool
	last   time.Time
	// rejected counts the rejections since the previous log record.
	rejected int
}

// flowLimiter bounds the concurrent guest flows of one sandbox network, with
// separate slots per transport, and logs rejections at most once per
// flowRejectLogInterval per transport. It never logs a destination or any
// payload. The clock is injected so tests can drive the log interval.
type flowLimiter struct {
	logger *slog.Logger
	now    func() time.Time
	mu     sync.Mutex
	slots  map[Transport]*flowSlots
}

func newFlowLimiter(logger *slog.Logger, limits flowLimits, now func() time.Time) *flowLimiter {
	return &flowLimiter{logger: logger, now: now, slots: map[Transport]*flowSlots{
		TransportTCP: {limit: limits.maxTCPConns},
		TransportUDP: {limit: limits.maxUDPFlows},
	}}
}

// acquire takes a slot of the transport. When the transport is full, it
// counts and maybe logs a rejection and reports false. The returned release
// function returns the slot, and a second call does nothing, so every path
// of a flow can release without coordinating.
func (l *flowLimiter) acquire(t Transport) (release func(), ok bool) {
	l.mu.Lock()
	s := l.slots[t]
	if s.inUse < s.limit {
		s.inUse++
		l.mu.Unlock()
		var once sync.Once
		return func() {
			once.Do(func() {
				l.mu.Lock()
				s.inUse--
				l.mu.Unlock()
			})
		}, true
	}
	s.rejected++
	now := l.now()
	if s.logged && now.Sub(s.last) < flowRejectLogInterval {
		l.mu.Unlock()
		return nil, false
	}
	count, limit := s.rejected, s.limit
	s.rejected = 0
	s.logged = true
	s.last = now
	l.mu.Unlock()
	l.logger.Warn("sandbox network stack at capacity; rejected flows",
		slog.String("transport", string(t)),
		slog.Int("rejected", count),
		slog.Int("limit", limit))
	return nil, false
}

// inUse returns the number of slots of the transport that are taken.
func (l *flowLimiter) inUse(t Transport) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.slots[t].inUse
}

// Network is the stack of one sandbox.
type Network struct {
	opts   Options
	logger *slog.Logger
	stack  *stack.Stack
	sw     *tap.Switch
	dns    *dns.Server
	dhcp   *dhcp.Server
	udpLn  *gonet.UDPConn
	tcpLn  *gonet.TCPListener
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	mu     sync.Mutex
	closed bool
	conns  map[io.Closer]bool
	// limiter bounds the guest TCP connections and UDP flows.
	limiter *flowLimiter
}

// RandomMAC returns a random locally administered unicast MAC address.
func RandomMAC() (net.HardwareAddr, error) {
	mac := make(net.HardwareAddr, 6)
	if _, err := rand.Read(mac); err != nil {
		return nil, err
	}
	mac[0] = mac[0]&^0x01 | 0x02
	return mac, nil
}

// New starts the stack of one sandbox.
func New(opts Options) (*Network, error) {
	if opts.Filter == nil {
		return nil, errors.New("netstack: a filter is required")
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.dial == nil {
		var d net.Dialer
		opts.dial = d.DialContext
	}
	opts.limits = opts.limits.withDefaults()
	gatewayMAC, err := RandomMAC()
	if err != nil {
		return nil, err
	}
	subnet := netip.PrefixFrom(opts.Link.Gateway, opts.Link.Bits).Masked()
	pool, err := tap.NewIPPool(subnet.String())
	if err != nil {
		return nil, err
	}
	if err := pool.Reserve(opts.Link.Gateway.String(), gatewayMAC.String()); err != nil {
		return nil, err
	}
	if err := pool.Reserve(opts.Link.Guest.String(), opts.GuestMAC.String()); err != nil {
		return nil, err
	}
	link, err := tap.NewLinkEndpoint(false, MTU, gatewayMAC.String(), opts.Link.Gateway.String(), nil)
	if err != nil {
		return nil, err
	}
	sw := tap.NewSwitch(false)
	link.Connect(sw)
	sw.Connect(link)

	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if terr := s.CreateNIC(nicID, link); terr != nil {
		return nil, fmt.Errorf("netstack: create NIC: %s", terr)
	}
	gw := tcpip.AddrFrom4(opts.Link.Gateway.As4())
	if terr := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{
		Protocol:          ipv4.ProtocolNumber,
		AddressWithPrefix: gw.WithPrefix(),
	}, stack.AddressProperties{}); terr != nil {
		s.Destroy()
		return nil, fmt.Errorf("netstack: add gateway address: %s", terr)
	}
	// Spoofing and promiscuous mode let the stack accept the guest's
	// packets to any destination and answer from that destination, which
	// is how the forwarders terminate guest connections.
	s.SetSpoofing(nicID, true)
	s.SetPromiscuousMode(nicID, true)
	tsub, _ := tcpip.NewSubnet(tcpip.AddrFrom4(subnet.Addr().As4()), tcpip.MaskFromBytes(net.CIDRMask(subnet.Bits(), 32)))
	s.SetRouteTable([]tcpip.Route{{Destination: tsub, NIC: nicID}})

	ctx, cancel := context.WithCancel(context.Background())
	n := &Network{opts: opts, logger: opts.Logger.With("sandbox_id", opts.SandboxID), stack: s, sw: sw,
		ctx: ctx, cancel: cancel, conns: map[io.Closer]bool{}}
	n.limiter = newFlowLimiter(n.logger, opts.limits, time.Now)

	tcpFwd := tcp.NewForwarder(s, 0, maxInFlightTCP, n.forwardTCP)
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		// The gateway's own TCP port 53 is the DNS listener.
		if id.LocalAddress == gw && id.LocalPort == 53 {
			return false
		}
		return tcpFwd.HandlePacket(id, pkt)
	})
	udpFwd := udp.NewForwarder(s, n.forwardUDP)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, func(id stack.TransportEndpointID, pkt *stack.PacketBuffer) bool {
		// DHCP and DNS of the gateway have their own endpoints.
		if id.LocalAddress == gw || id.LocalAddress == header.IPv4Broadcast {
			return false
		}
		return udpFwd.HandlePacket(id, pkt)
	})

	if err := n.startServices(gw, pool); err != nil {
		n.Close()
		return nil, err
	}
	n.logger.Info("sandbox network stack started", "gateway", opts.Link.Gateway.String(), "guest", opts.Link.Guest.String())
	return n, nil
}

func (n *Network) startServices(gw tcpip.Address, pool *tap.IPPool) error {
	udpLn, err := gonet.DialUDP(n.stack, &tcpip.FullAddress{NIC: nicID, Addr: gw, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("netstack: DNS UDP listener: %w", err)
	}
	tcpLn, err := gonet.ListenTCP(n.stack, tcpip.FullAddress{NIC: nicID, Addr: gw, Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		udpLn.Close()
		return fmt.Errorf("netstack: DNS TCP listener: %w", err)
	}
	n.udpLn, n.tcpLn = udpLn, tcpLn
	server, err := dns.New(udpLn, tcpLn, nil)
	if err != nil {
		return fmt.Errorf("netstack: DNS server: %w", err)
	}
	n.dns = server
	n.wg.Add(2)
	go func() { defer n.wg.Done(); _ = server.Serve() }()
	go func() { defer n.wg.Done(); _ = server.ServeTCP() }()

	cfg := &types.Configuration{
		MTU:              MTU,
		Subnet:           netip.PrefixFrom(n.opts.Link.Gateway, n.opts.Link.Bits).Masked().String(),
		GatewayIP:        n.opts.Link.Gateway.String(),
		DHCPStaticLeases: map[string]string{n.opts.Link.Guest.String(): n.opts.GuestMAC.String()},
	}
	d, err := dhcp.New(cfg, n.stack, pool)
	if err != nil {
		return fmt.Errorf("netstack: DHCP server: %w", err)
	}
	n.dhcp = d
	n.wg.Add(1)
	go func() { defer n.wg.Done(); _ = d.Serve() }()
	return nil
}

// Serve carries the guest's Ethernet frames between conn and the stack
// until conn fails, ctx ends or the network closes. A later Serve may
// attach a new connection (for example after the tunnel reconnected).
func (n *Network) Serve(ctx context.Context, conn net.Conn, framing Framing) error {
	proto, err := framing.protocol()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
		case <-n.ctx.Done():
		}
		conn.Close()
	}()
	err = n.sw.Accept(ctx, conn, proto)
	if n.ctx.Err() != nil || ctx.Err() != nil {
		return nil
	}
	return err
}

// startProxy registers the connections of one forwarded flow, so that
// Close closes them, and runs relay in a goroutine that Close waits for.
// The registration and the WaitGroup increment happen under the lock that
// Close takes to mark the network closed, so Close either sees every
// connection and the goroutine or none of them. When the network is closed
// already, startProxy closes the connections and reports false.
func (n *Network) startProxy(relay func(), conns ...io.Closer) bool {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
		return false
	}
	for _, c := range conns {
		n.conns[c] = true
	}
	n.wg.Add(1)
	n.mu.Unlock()
	go func() {
		defer n.wg.Done()
		defer func() {
			n.mu.Lock()
			for _, c := range conns {
				delete(n.conns, c)
			}
			n.mu.Unlock()
		}()
		relay()
	}()
	return true
}

func destination(addr tcpip.Address) netip.Addr {
	a, _ := netip.AddrFromSlice(addr.AsSlice())
	return a
}

// forwardTCP opens the host connection of one guest TCP connection after
// the filter allowed its destination and a connection slot is free, and then
// completes the guest's handshake. A blocked or unreachable destination, and
// a connection beyond the limit, get a reset. The slot is held through the
// host dial and the relay and returns on every path.
func (n *Network) forwardTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	dst := destination(id.LocalAddress)
	if !n.opts.Filter.Allows(dst) {
		n.logger.Debug("guest TCP connection blocked by the network policy", "destination", dst.String(), "port", id.LocalPort)
		r.Complete(true)
		return
	}
	release, ok := n.limiter.acquire(TransportTCP)
	if !ok {
		r.Complete(true)
		return
	}
	ctx, cancel := context.WithTimeout(n.ctx, tcpConnectTimeout)
	out, err := n.opts.dial(ctx, "tcp", net.JoinHostPort(dst.String(), strconv.Itoa(int(id.LocalPort))))
	cancel()
	if err != nil {
		release()
		r.Complete(true)
		return
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	r.Complete(false)
	if terr != nil {
		out.Close()
		release()
		return
	}
	in := gonet.NewTCPConn(&wq, ep)
	halfCloseIdle := n.opts.limits.tcpHalfCloseIdle
	if !n.startProxy(func() {
		defer release()
		proxy(in, out, halfCloseIdle)
	}, in, out) {
		release()
	}
}

// halfCloseGuard is shared by both directions of a proxy. Once one direction
// has ended, every read and write of the other direction renews a deadline of
// idle, so that direction ends after idle without data.
type halfCloseGuard struct {
	ended atomic.Bool
	idle  time.Duration
}

type guardedReader struct {
	net.Conn
	g *halfCloseGuard
}

func (r guardedReader) Read(p []byte) (int, error) {
	if r.g.ended.Load() {
		_ = r.Conn.SetReadDeadline(time.Now().Add(r.g.idle))
	}
	return r.Conn.Read(p)
}

type guardedWriter struct {
	net.Conn
	g *halfCloseGuard
}

func (w guardedWriter) Write(p []byte) (int, error) {
	if w.g.ended.Load() {
		_ = w.Conn.SetWriteDeadline(time.Now().Add(w.g.idle))
	}
	return w.Conn.Write(p)
}

// proxy copies both directions until both ended; a side's end closes the
// other side's write direction. Once one direction has ended, the other one
// ends after halfCloseIdle without data (any data renews the time), and then
// proxy closes both sides, so a remote side that stays silent cannot hold the
// flow forever. A connection with both directions open has no idle time. A
// halfCloseIdle of zero or less disables the deadline.
func proxy(a, b net.Conn, halfCloseIdle time.Duration) {
	g := &halfCloseGuard{idle: halfCloseIdle}
	done := make(chan struct{}, 2)
	cp := func(dst, src net.Conn) {
		in, out := io.Reader(src), io.Writer(dst)
		if halfCloseIdle > 0 {
			in, out = guardedReader{src, g}, guardedWriter{dst, g}
		}
		_, _ = io.Copy(out, in)
		if cw, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		} else {
			_ = dst.Close()
		}
		if halfCloseIdle > 0 {
			// The other direction reads from dst and writes to src. Set the
			// flag first: a read or write that starts later renews its own
			// deadline, and the deadlines set here wake the ones that are
			// blocked already.
			g.ended.Store(true)
			deadline := time.Now().Add(halfCloseIdle)
			_ = dst.SetReadDeadline(deadline)
			_ = src.SetWriteDeadline(deadline)
		}
		done <- struct{}{}
	}
	go cp(a, b)
	go cp(b, a)
	<-done
	<-done
	a.Close()
	b.Close()
}

// forwardUDP opens the host flow of one guest UDP flow (by its source and
// destination) after the filter allowed its destination and a flow slot is
// free. The datagram of a new flow beyond the limit is dropped and the guest
// application retries; datagrams of existing flows reach their endpoints
// directly and are not affected. The slot is held until the relay ends.
func (n *Network) forwardUDP(r *udp.ForwarderRequest) bool {
	id := r.ID()
	dst := destination(id.LocalAddress)
	if !n.opts.Filter.Allows(dst) {
		n.logger.Debug("guest UDP flow blocked by the network policy", "destination", dst.String(), "port", id.LocalPort)
		return true
	}
	release, ok := n.limiter.acquire(TransportUDP)
	if !ok {
		return true
	}
	var wq waiter.Queue
	ep, terr := r.CreateEndpoint(&wq)
	if terr != nil {
		release()
		return true
	}
	in := gonet.NewUDPConn(&wq, ep)
	out, err := n.opts.dial(n.ctx, "udp", net.JoinHostPort(dst.String(), strconv.Itoa(int(id.LocalPort))))
	if err != nil {
		in.Close()
		release()
		return true
	}
	idle := n.opts.limits.udpIdle
	if !n.startProxy(func() {
		defer release()
		proxyUDP(in, out, idle)
	}, in, out) {
		release()
	}
	return true
}

// idleTimer tracks the shared idle time of one UDP flow. It stores the
// elapsed time since the flow started, never a wall-clock timestamp, so a
// forward or backward step of the system clock cannot shorten or lengthen the
// idle lifetime. The elapsed source is injected so tests can drive it.
type idleTimer struct {
	elapsed func() time.Duration
	idle    time.Duration
	// last is the elapsed time of the latest datagram, in nanoseconds.
	last atomic.Int64
}

// newMonotonicElapsed returns an elapsed source based on the monotonic clock
// reading that time.Now carries; time.Since ignores wall-clock steps.
func newMonotonicElapsed() func() time.Duration {
	start := time.Now()
	return func() time.Duration { return time.Since(start) }
}

func newIdleTimer(idle time.Duration, elapsed func() time.Duration) *idleTimer {
	t := &idleTimer{elapsed: elapsed, idle: idle}
	t.last.Store(int64(elapsed()))
	return t
}

// touch records a datagram in either direction. The recorded time never moves
// backward, so a slower goroutine cannot undo a newer datagram.
func (t *idleTimer) touch() {
	now := int64(t.elapsed())
	for {
		old := t.last.Load()
		if now <= old || t.last.CompareAndSwap(old, now) {
			return
		}
	}
}

// remaining returns how long the flow may stay idle; zero or less means the
// flow is idle for at least the idle time.
func (t *idleTimer) remaining() time.Duration {
	return t.idle - (t.elapsed() - time.Duration(t.last.Load()))
}

// proxyUDP relays the datagrams of one flow until neither direction
// carried a datagram for idle, or a side fails. Both directions share one
// activity time, so a flow whose traffic goes mostly one way stays open
// while that direction is active. The idle time follows the monotonic clock,
// not the wall clock.
func proxyUDP(in, out net.Conn, idle time.Duration) {
	proxyUDPWithClock(in, out, idle, newMonotonicElapsed())
}

func proxyUDPWithClock(in, out net.Conn, idle time.Duration, elapsed func() time.Duration) {
	timer := newIdleTimer(idle, elapsed)
	done := make(chan struct{}, 2)
	relay := func(dst, src net.Conn) {
		buf := make([]byte, 65535)
		for {
			// The read waits until the flow has been idle for idle since its
			// last datagram in either direction.
			remaining := timer.remaining()
			if remaining <= 0 {
				break
			}
			// time.Now carries a monotonic reading, so the deadline does not
			// follow wall-clock steps either.
			_ = src.SetReadDeadline(time.Now().Add(remaining))
			k, err := src.Read(buf)
			if err != nil {
				var ne net.Error
				if errors.As(err, &ne) && ne.Timeout() {
					// The other direction may have moved the deadline.
					continue
				}
				break
			}
			timer.touch()
			if _, err := dst.Write(buf[:k]); err != nil {
				break
			}
		}
		done <- struct{}{}
	}
	go relay(out, in)
	go relay(in, out)
	<-done
	in.Close()
	out.Close()
	<-done
}

// Close stops the stack and closes every forwarded connection.
func (n *Network) Close() {
	n.mu.Lock()
	if n.closed {
		n.mu.Unlock()
		return
	}
	n.closed = true
	conns := n.conns
	n.conns = map[io.Closer]bool{}
	n.mu.Unlock()
	n.cancel()
	for c := range conns {
		c.Close()
	}
	if n.udpLn != nil {
		n.udpLn.Close()
	}
	if n.tcpLn != nil {
		n.tcpLn.Close()
	}
	if n.dhcp != nil {
		_ = n.dhcp.Underlying.Close()
	}
	n.stack.Close()
	n.wg.Wait()
	n.stack.Destroy()
	n.logger.Info("sandbox network stack stopped")
}
