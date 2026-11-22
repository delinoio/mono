// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bytes"
	"encoding/binary"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDNSForwarder(t *testing.T) {
	// Upstream: UDP and TCP echo with a marker, answering at once. The helpers
	// retry with another candidate port when one of the two listeners cannot
	// bind it.
	up := startGatedUpstream(t)
	up.open()
	f := startTestDNS(t, up.addr, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: DefaultDNSMaxUDPQueries, maxTCPConns: DefaultDNSMaxTCPConns})
	defer f.Close()
	c, err := net.Dial("udp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	c.Write([]byte("query"))
	buf := make([]byte, 64)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "ok:query" {
		t.Fatalf("udp answer %q %v", buf[:n], err)
	}
	tc, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	tc.SetDeadline(time.Now().Add(5 * time.Second))
	tc.Write([]byte{0, 3, 'a', 'b', 'c'})
	got := make([]byte, 5)
	if _, err := io.ReadFull(tc, got); err != nil || string(got[2:]) != "abc" {
		t.Fatalf("tcp answer %q %v", got, err)
	}
}

func TestHostResolver(t *testing.T) {
	p := filepath.Join(t.TempDir(), "resolv.conf")
	os.WriteFile(p, []byte("# x\nsearch lan\nnameserver 192.0.2.53\nnameserver 192.0.2.54\n"), 0o644)
	if got := HostResolver(p); got != "192.0.2.53:53" {
		t.Fatalf("resolver %s", got)
	}
	for _, c := range []struct{ conf, want string }{
		{"nameserver fe80::53%eth-test\nnameserver 192.0.2.54\n", "[fe80::53%eth-test]:53"},
		{"nameserver 2001:db8::53\nnameserver 192.0.2.54\n", "[2001:db8::53]:53"},
	} {
		os.WriteFile(p, []byte(c.conf), 0o644)
		if got := HostResolver(p); got != c.want {
			t.Fatalf("resolver %q: got %s want %s", c.conf, got, c.want)
		}
	}
	if got := HostResolver(filepath.Join(t.TempDir(), "missing")); got != "127.0.0.53:53" {
		t.Fatalf("fallback %s", got)
	}
}

// gatedUpstream is a fake upstream whose UDP and TCP handlers block until
// release is closed, and that counts the work it receives.
type gatedUpstream struct {
	addr       string
	release    chan struct{}
	once       sync.Once
	udpQueries atomic.Int32
	tcpConns   atomic.Int32
}

func startGatedUpstream(t *testing.T) *gatedUpstream {
	t.Helper()
	// The UDP and TCP listeners need the same port number, which can be taken
	// or excluded for one of them: retry with another candidate port.
	var up *net.UDPConn
	var upTCP net.Listener
	for attempt := 0; ; attempt++ {
		var err error
		upTCP, err = net.Listen("tcp", candidateAddr().String())
		if err == nil {
			if up, err = net.ListenUDP("udp", net.UDPAddrFromAddrPort(upTCP.Addr().(*net.TCPAddr).AddrPort())); err == nil {
				break
			}
			upTCP.Close()
		}
		if attempt == 20 {
			t.Fatal(err)
		}
	}
	g := &gatedUpstream{addr: up.LocalAddr().String(), release: make(chan struct{})}
	t.Cleanup(func() {
		g.open()
		up.Close()
		upTCP.Close()
	})
	go func() {
		buf := make([]byte, 512)
		for {
			n, from, err := up.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			g.udpQueries.Add(1)
			msg := append([]byte("ok:"), buf[:n]...)
			go func() {
				<-g.release
				up.WriteToUDPAddrPort(msg, from)
			}()
		}
	}()
	go func() {
		for {
			c, err := upTCP.Accept()
			if err != nil {
				return
			}
			g.tcpConns.Add(1)
			go func() {
				defer c.Close()
				var l [2]byte
				if _, err := io.ReadFull(c, l[:]); err != nil {
					return
				}
				msg := make([]byte, binary.BigEndian.Uint16(l[:]))
				if _, err := io.ReadFull(c, msg); err != nil {
					return
				}
				<-g.release
				c.Write(append(l[:], msg...))
			}()
		}
	}()
	return g
}

// startTestDNS is startDNS with retries: the forwarder listens on one port
// number for UDP and TCP, and a candidate port can be taken or excluded for
// one of them.
func startTestDNS(t *testing.T, upstream string, logger *slog.Logger, limits dnsLimits) *DNSForwarder {
	t.Helper()
	for attempt := 0; ; attempt++ {
		f, err := startDNS(candidateAddr(), upstream, logger, limits)
		if err == nil {
			return f
		}
		if attempt == 20 {
			t.Fatal(err)
		}
	}
}

// candidateAddr returns a loopback address with a random port of the dynamic
// range (49152-65535) for a test listener that needs the same port number for
// UDP and TCP. A kernel-chosen port is no good candidate on Windows: Windows
// hands out ephemeral ports in sequence, and Hyper-V, WSL and WinNAT exclude
// blocks of port numbers from that range (netsh int ipv4 show
// excludedportrange protocol=tcp, and protocol=udp), so once one candidate
// falls into such a block, every following kernel-chosen candidate fails with
// an access error in the same block.
func candidateAddr() netip.AddrPort {
	return netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), uint16(49152+rand.IntN(65536-49152)))
}

func (g *gatedUpstream) open() { g.once.Do(func() { close(g.release) }) }

func waitForCount(t *testing.T, what string, c *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for c.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("%s: got %d want %d", what, c.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func dnsUDPQuery(t *testing.T, f *DNSForwarder, q string) net.Conn {
	t.Helper()
	c, err := net.Dial("udp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(q)); err != nil {
		t.Fatal(err)
	}
	return c
}

func dnsTCPConn(t *testing.T, f *DNSForwarder, q string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append([]byte{0, byte(len(q))}, q...)); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestDNSForwarderBoundsUDPQueries(t *testing.T) {
	g := startGatedUpstream(t)
	var logs bytes.Buffer
	f := startTestDNS(t, g.addr, slog.New(slog.NewJSONHandler(&logs, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	first := dnsUDPQuery(t, f, "first")
	waitForCount(t, "first udp query", &g.udpQueries, 1)

	// The slot is held: the next queries start no upstream work.
	dropped := dnsUDPQuery(t, f, "secret-payload-1")
	dnsUDPQuery(t, f, "secret-payload-2")
	time.Sleep(200 * time.Millisecond)
	if n := g.udpQueries.Load(); n != 1 {
		t.Fatalf("upstream udp queries while at capacity: %d", n)
	}

	// The TCP limit is separate: a TCP connection still reaches the upstream.
	tcp := dnsTCPConn(t, f, "tcp")
	waitForCount(t, "tcp conns", &g.tcpConns, 1)

	g.open()
	buf := make([]byte, 64)
	if n, err := first.Read(buf); err != nil || string(buf[:n]) != "ok:first" {
		t.Fatalf("admitted udp answer %q %v", buf[:n], err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(tcp, got); err != nil || string(got[2:]) != "tcp" {
		t.Fatalf("admitted tcp answer %q %v", got, err)
	}
	dropped.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if n, err := dropped.Read(buf); err == nil {
		t.Fatalf("dropped query got an answer %q", buf[:n])
	}

	// After the release the next query forwards, with its content unchanged.
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dnsUDPQuery(t, f, "after")
		c.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		if n, err := c.Read(buf); err == nil {
			if string(buf[:n]) != "ok:after" {
				t.Fatalf("answer after release %q", buf[:n])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer after the slot was released")
		}
	}

	// Rejections are logged with transport and count, without payload, and
	// at most once per interval.
	out := logs.String()
	if got := strings.Count(out, "rejected work"); got != 1 {
		t.Fatalf("rejection log records: %d\n%s", got, out)
	}
	if !strings.Contains(out, `"transport":"udp"`) || !strings.Contains(out, `"rejected":1`) {
		t.Fatalf("rejection log lacks transport or count: %s", out)
	}
	if strings.Contains(out, "secret-payload") {
		t.Fatalf("rejection log contains a payload: %s", out)
	}
}

func TestDNSForwarderBoundsTCPConns(t *testing.T) {
	g := startGatedUpstream(t)
	f := startTestDNS(t, g.addr, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	first := dnsTCPConn(t, f, "first")
	waitForCount(t, "first tcp conn", &g.tcpConns, 1)

	// The slot is held: the next connection is closed and the upstream sees
	// no dial.
	excess, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer excess.Close()
	excess.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := excess.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("excess connection was not closed: %v", err)
	}
	if n := g.tcpConns.Load(); n != 1 {
		t.Fatalf("upstream tcp dials while at capacity: %d", n)
	}

	// The UDP limit is separate: a UDP query still reaches the upstream.
	udp := dnsUDPQuery(t, f, "udp")
	waitForCount(t, "udp queries", &g.udpQueries, 1)

	g.open()
	got := make([]byte, 7)
	if _, err := io.ReadFull(first, got); err != nil || string(got[2:]) != "first" {
		t.Fatalf("admitted tcp answer %q %v", got, err)
	}
	buf := make([]byte, 64)
	if n, err := udp.Read(buf); err != nil || string(buf[:n]) != "ok:udp" {
		t.Fatalf("admitted udp answer %q %v", buf[:n], err)
	}

	// A TCP slot is held until the guest closes the connection. After that
	// the next connection forwards.
	first.Close()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dnsTCPConn(t, f, "after")
		got := make([]byte, 7)
		if _, err := io.ReadFull(c, got); err == nil {
			if string(got[2:]) != "after" {
				t.Fatalf("answer after release %q", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no answer after the slot was released")
		}
	}
}

// startTCPUpstream starts a TCP-only fake upstream that runs handle for each
// connection. The cleanup closes the listener and every connection, so a
// handler that blocks ends with the test.
func startTCPUpstream(t *testing.T, handle func(c net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var conns []net.Conn
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			go handle(c)
		}
	}()
	return l.Addr().String()
}

// readDNSFrame reads one length-prefixed message and returns its payload.
func readDNSFrame(c net.Conn) ([]byte, error) {
	var l [2]byte
	if _, err := io.ReadFull(c, l[:]); err != nil {
		return nil, err
	}
	msg := make([]byte, binary.BigEndian.Uint16(l[:]))
	_, err := io.ReadFull(c, msg)
	return msg, err
}

// requireDNSAnswer reads one length-prefixed answer from the guest side.
func requireDNSAnswer(t *testing.T, c net.Conn, want string) {
	t.Helper()
	got, err := readDNSFrame(c)
	if err != nil || string(got) != want {
		t.Fatalf("answer %q %v, want %q", got, err, want)
	}
}

// requireTCPSlotFree checks that a one-slot forwarder admits and answers a
// new connection, which proves the slot of an earlier connection is free.
func requireTCPSlotFree(t *testing.T, f *DNSForwarder) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c := dnsTCPConn(t, f, "next")
		if got, err := readDNSFrame(c); err == nil {
			if string(got) != "ok:next" {
				t.Fatalf("answer on the freed slot %q", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("tcp slot was not released")
		}
	}
}

// answerThenClose reads one query, answers it with "ok:" plus the query and
// closes the connection, like an upstream that closes first.
func answerThenClose(c net.Conn) {
	defer c.Close()
	msg, err := readDNSFrame(c)
	if err != nil {
		return
	}
	answerFrame(c, msg)
}

// answerFrame writes the answer "ok:" plus the query as one length-prefixed
// message.
func answerFrame(c net.Conn, query []byte) {
	ans := append([]byte("ok:"), query...)
	c.Write(append([]byte{byte(len(ans) >> 8), byte(len(ans))}, ans...))
}

func TestDNSForwarderUpstreamClosesFirst(t *testing.T) {
	// The default 30-second deadline stays in place: the guest must see EOF
	// long before it.
	up := startTCPUpstream(t, answerThenClose)
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	c := dnsTCPConn(t, f, "first")
	requireDNSAnswer(t, c, "ok:first")
	// The guest keeps its side open and sends nothing more; the upstream
	// closing ends the connection toward the guest.
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("guest connection did not end after the upstream closed: %v", err)
	}
	requireTCPSlotFree(t, f)
}

func TestDNSForwarderGuestClosesFirst(t *testing.T) {
	sawEOF := make(chan struct{}, 1)
	up := startTCPUpstream(t, func(c net.Conn) {
		defer c.Close()
		msg, err := readDNSFrame(c)
		if err != nil {
			return
		}
		// The follow-up query of requireTCPSlotFree gets its answer at once.
		if string(msg) != "first" {
			answerFrame(c, msg)
			return
		}
		// Wait for the forwarder to half-close the upstream, then answer:
		// the guest's half-close must not drop this pending answer.
		if _, err := io.Copy(io.Discard, c); err != nil {
			return
		}
		sawEOF <- struct{}{}
		answerFrame(c, msg)
	})
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	c := dnsTCPConn(t, f, "first")
	c.(*net.TCPConn).CloseWrite()
	select {
	case <-sawEOF:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream did not see the guest close")
	}
	requireDNSAnswer(t, c, "ok:first")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("guest connection did not end after the answer: %v", err)
	}
	requireTCPSlotFree(t, f)
}

func TestDNSForwarderEndsSilentConnAtDeadline(t *testing.T) {
	const deadline = 300 * time.Millisecond
	// The upstream reads the query and then stays silent and open.
	up := startTCPUpstream(t, func(c net.Conn) {
		readDNSFrame(c)
		io.Copy(io.Discard, c)
	})
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, tcpDeadline: deadline})
	defer f.Close()

	start := time.Now()
	c := dnsTCPConn(t, f, "silent")
	c.SetReadDeadline(time.Now().Add(deadline + 2*time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent connection delivered data")
	}
	elapsed := time.Since(start)
	if elapsed < deadline {
		t.Fatalf("connection ended after %v, before the %v deadline", elapsed, deadline)
	}
	if elapsed > deadline+2*time.Second {
		t.Fatalf("connection ended after %v, long after the %v deadline", elapsed, deadline)
	}
}

func TestDNSForwarderLimitsAreIndependentPerForwarder(t *testing.T) {
	g := startGatedUpstream(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	a := startTestDNS(t, g.addr, logger, dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer a.Close()
	b := startTestDNS(t, g.addr, logger, dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer b.Close()

	dnsUDPQuery(t, a, "a1")
	waitForCount(t, "udp queries", &g.udpQueries, 1)
	dnsUDPQuery(t, b, "b1")
	waitForCount(t, "udp queries of the other forwarder", &g.udpQueries, 2)
	g.open()
}
