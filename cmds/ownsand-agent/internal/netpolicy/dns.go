// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"
)

// DNSTransport names a transport of the DNS forwarder.
type DNSTransport string

const (
	DNSTransportUDP DNSTransport = "udp"
	DNSTransportTCP DNSTransport = "tcp"
)

// Default limits of concurrent forwarding work for one forwarder (one
// sandbox). Each UDP query holds one upstream socket and each TCP connection
// holds one guest and one upstream connection, so the limits bound the
// sockets and goroutines that one guest can make the host open. The two
// transports have separate limits, so a flood on one never starves the other.
const (
	DefaultDNSMaxUDPQueries = 64
	DefaultDNSMaxTCPConns   = 32
)

// DefaultDNSTCPDeadline is the upper bound of one forwarded TCP connection,
// counted from its admission and covering the upstream dial and the copy: the
// forwarder ends the connection at this deadline even when both sides stay
// open.
const DefaultDNSTCPDeadline = 30 * time.Second

// dnsRejectLogInterval is the minimum time between two rejection log records
// of one transport. Rejections in between only increase the count that the
// next record reports.
const dnsRejectLogInterval = 5 * time.Second

// dnsLimits holds the concurrency limits of one forwarder.
type dnsLimits struct {
	maxUDPQueries int
	maxTCPConns   int
	// tcpDeadline bounds one TCP connection (dial and copy). Zero means
	// DefaultDNSTCPDeadline; tests set a short value.
	tcpDeadline time.Duration
	// afterAdmit, when set, runs in serveTCP after a connection took its TCP
	// slot and before it is registered. It is a test hook that holds the
	// accept path open at the point where Close can overtake it; production
	// never sets it.
	afterAdmit func()
	// dial, when set, replaces the upstream TCP dial. It is a test hook that
	// makes a slow dial reproducible; production leaves it nil and dials with
	// a net.Dialer.
	dial func(ctx context.Context, network, address string) (net.Conn, error)
}

// dnsRejectLog logs rejections of one transport at most once per interval,
// with the number of rejections since the previous record. It never logs a
// payload.
type dnsRejectLog struct {
	mu       sync.Mutex
	last     time.Time
	rejected int
}

// note counts one rejection and logs when the interval has passed.
func (r *dnsRejectLog) note(logger *slog.Logger, transport DNSTransport, limit int) {
	r.mu.Lock()
	r.rejected++
	now := time.Now()
	if !r.last.IsZero() && now.Sub(r.last) < dnsRejectLogInterval {
		r.mu.Unlock()
		return
	}
	count := r.rejected
	r.rejected = 0
	r.last = now
	r.mu.Unlock()
	logger.Warn("dns forwarder at capacity; rejected work",
		slog.String("transport", string(transport)),
		slog.Int("rejected", count),
		slog.Int("limit", limit))
}

// DNSForwarder serves DNS to one guest at its gateway address and resolves
// through the host resolver. It forwards whole messages without parsing
// them, over UDP and TCP. It runs at most a fixed number of concurrent UDP
// queries and TCP connections: a UDP query beyond the limit is dropped (the
// guest resolver retries) and a TCP connection beyond the limit is closed
// before any upstream work starts.
type DNSForwarder struct {
	udp      *net.UDPConn
	tcp      *net.TCPListener
	upstream string
	logger   *slog.Logger
	wg       sync.WaitGroup
	closed   chan struct{}
	mu       sync.Mutex
	conns    map[net.Conn]bool
	udpSlots chan struct{}
	tcpSlots chan struct{}
	udpRej   dnsRejectLog
	tcpRej   dnsRejectLog
	limits   dnsLimits
}

// HostResolver returns the first name server of /etc/resolv.conf as
// host:port (keeping the interface zone of a scoped IPv6 address, for
// example [fe80::53%eth0]:53), or 127.0.0.53:53 (systemd-resolved) when none is listed.
func HostResolver(path string) string {
	f, err := os.Open(path)
	if err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			fields := strings.Fields(sc.Text())
			if len(fields) >= 2 && fields[0] == "nameserver" {
				if a, err := netip.ParseAddr(fields[1]); err == nil {
					return netip.AddrPortFrom(a, 53).String()
				}
			}
		}
	}
	return "127.0.0.53:53"
}

// StartDNS listens on listen (UDP and TCP) and forwards to upstream.
func StartDNS(listen netip.AddrPort, upstream string, logger *slog.Logger) (*DNSForwarder, error) {
	return startDNS(listen, upstream, logger, dnsLimits{maxUDPQueries: DefaultDNSMaxUDPQueries, maxTCPConns: DefaultDNSMaxTCPConns})
}

// startDNS is StartDNS with explicit concurrency limits (tests set small ones).
func startDNS(listen netip.AddrPort, upstream string, logger *slog.Logger, limits dnsLimits) (*DNSForwarder, error) {
	if limits.tcpDeadline <= 0 {
		limits.tcpDeadline = DefaultDNSTCPDeadline
	}
	// TCP is bound first, and UDP takes the port number of the TCP listener.
	// For a fixed port (the agent uses 53) the order does not matter; for port
	// 0, the kernel chooses the TCP port. The tests pass random fixed ports,
	// because a kernel-chosen port can be excluded for UDP on Windows.
	tcp, err := net.ListenTCP("tcp", net.TCPAddrFromAddrPort(listen))
	if err != nil {
		return nil, err
	}
	udp, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.AddrPortFrom(listen.Addr(), tcp.Addr().(*net.TCPAddr).AddrPort().Port())))
	if err != nil {
		tcp.Close()
		return nil, err
	}
	f := &DNSForwarder{udp: udp, tcp: tcp, upstream: upstream, logger: logger, closed: make(chan struct{}), conns: map[net.Conn]bool{},
		udpSlots: make(chan struct{}, limits.maxUDPQueries), tcpSlots: make(chan struct{}, limits.maxTCPConns), limits: limits}
	f.wg.Add(2)
	go f.serveUDP()
	go f.serveTCP()
	return f, nil
}

// Addr returns the UDP listen address.
func (f *DNSForwarder) Addr() netip.AddrPort {
	return f.udp.LocalAddr().(*net.UDPAddr).AddrPort()
}

// Close stops the forwarder.
func (f *DNSForwarder) Close() {
	select {
	case <-f.closed:
		return
	default:
	}
	close(f.closed)
	f.udp.Close()
	f.tcp.Close()
	f.mu.Lock()
	for c := range f.conns {
		c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

func (f *DNSForwarder) serveUDP() {
	defer f.wg.Done()
	buf := make([]byte, 65535)
	for {
		n, from, err := f.udp.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		// Admit before copying the query or opening an upstream socket.
		select {
		case f.udpSlots <- struct{}{}:
		default:
			f.udpRej.note(f.logger, DNSTransportUDP, f.limits.maxUDPQueries)
			continue
		}
		query := append([]byte(nil), buf[:n]...)
		f.wg.Add(1)
		go func() {
			defer f.wg.Done()
			defer func() { <-f.udpSlots }()
			c, err := net.DialTimeout("udp", f.upstream, 5*time.Second)
			if err != nil {
				return
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if _, err := c.Write(query); err != nil {
				return
			}
			resp := make([]byte, 65535)
			m, err := c.Read(resp)
			if err != nil {
				return
			}
			f.udp.WriteToUDPAddrPort(resp[:m], from)
		}()
	}
}

func (f *DNSForwarder) serveTCP() {
	defer f.wg.Done()
	for {
		c, err := f.tcp.Accept()
		if err != nil {
			return
		}
		// Admit before dialing the upstream; close the excess connection.
		select {
		case f.tcpSlots <- struct{}{}:
		default:
			c.Close()
			f.tcpRej.note(f.logger, DNSTransportTCP, f.limits.maxTCPConns)
			continue
		}
		// One absolute deadline bounds the whole connection, dial and copy
		// together, counted from admission.
		deadline := time.Now().Add(f.limits.tcpDeadline)
		if f.limits.afterAdmit != nil {
			f.limits.afterAdmit()
		}
		// Register under the lock that Close holds while it closes the
		// registered connections, and check closed in the same critical
		// section: a connection accepted while Close ran is either closed by
		// Close or refused here, never left open for a handler that Close
		// would wait for until the TCP deadline.
		f.mu.Lock()
		select {
		case <-f.closed:
			f.mu.Unlock()
			c.Close()
			<-f.tcpSlots
			return
		default:
		}
		f.conns[c] = true
		f.wg.Add(1)
		f.mu.Unlock()
		go func() {
			defer f.wg.Done()
			defer func() { <-f.tcpSlots }()
			defer func() {
				f.mu.Lock()
				delete(f.conns, c)
				f.mu.Unlock()
				c.Close()
			}()
			ctx, cancel := context.WithDeadline(context.Background(), deadline)
			defer cancel()
			dial := f.limits.dial
			if dial == nil {
				var d net.Dialer
				dial = d.DialContext
			}
			up, err := dial(ctx, "tcp", f.upstream)
			if err != nil {
				return
			}
			defer up.Close()
			// The time spent dialing is already gone: both sides get the
			// remainder of the admission deadline, not a fresh budget.
			c.SetDeadline(deadline)
			up.SetDeadline(deadline)
			done := make(chan struct{})
			go func() {
				io.Copy(up, c)
				if tc, ok := up.(*net.TCPConn); ok {
					tc.CloseWrite()
				}
				close(done)
			}()
			io.Copy(c, up)
			// The upstream ended its answer direction (it closed, or the
			// deadline passed). Close the guest connection now: the guest
			// observes EOF, the guest-to-upstream copy above stops reading
			// and the TCP slot is released, instead of waiting for the guest
			// to close or for the deadline. A guest that closes first keeps
			// the path above: CloseWrite on the upstream lets a pending
			// answer still reach the guest.
			c.Close()
			<-done
		}()
	}
}
