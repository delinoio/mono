// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
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
	defer c.Close()
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
	defer tc.Close()
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

// pauseAfterAdmit returns a dnsLimits hook that blocks the first admitted
// connection in serveTCP until resume is called, and a channel that reports
// the pause.
func pauseAfterAdmit() (hook func(), paused <-chan struct{}, resume func()) {
	p := make(chan struct{})
	r := make(chan struct{})
	var once, rOnce sync.Once
	hook = func() {
		once.Do(func() {
			close(p)
			<-r
		})
	}
	return hook, p, func() { rOnce.Do(func() { close(r) }) }
}

// closeInBackground calls f.Close in a goroutine and returns a channel that
// closes when it returns.
func closeInBackground(f *DNSForwarder) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.Close()
	}()
	return done
}

func TestDNSForwarderRefusesConnAcceptedDuringClose(t *testing.T) {
	// A silent upstream would hold a registered connection for the whole
	// default 30-second deadline.
	up := startTCPUpstream(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	hook, paused, resume := pauseAfterAdmit()
	defer resume()
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 2, afterAdmit: hook})
	defer f.Close()

	c, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("serveTCP did not admit the connection")
	}
	// Close runs while the accept path holds an admitted, unregistered
	// connection. Once Close has closed the listener, let the accept path go on.
	done := closeInBackground(f)
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-f.closed:
		default:
			if time.Now().After(deadline) {
				t.Fatal("Close did not start")
			}
			time.Sleep(time.Millisecond)
			continue
		}
		break
	}
	resume()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for a connection accepted during Close")
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("connection accepted during Close stayed open: %v", err)
	}
	if n := len(f.tcpSlots); n != 0 {
		t.Fatalf("tcp slots in use after Close: %d", n)
	}
}

func TestDNSForwarderRefusesConnAcceptedDuringCloseAtCapacity(t *testing.T) {
	up := startTCPUpstream(t, func(c net.Conn) { io.Copy(io.Discard, c) })
	hook, paused, resume := pauseAfterAdmit()
	defer resume()
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, afterAdmit: hook})
	defer f.Close()

	// The only slot is held by the paused connection; a second connection is
	// refused at capacity and does not disturb the paused one.
	first, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("serveTCP did not admit the connection")
	}
	// serveTCP is paused, so it does not even accept the second connection;
	// it waits in the kernel queue until the loop resumes and refuses it.
	second, err := net.Dial("tcp", f.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	done := closeInBackground(f)
	time.Sleep(50 * time.Millisecond)
	resume()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close waited for connections accepted during Close")
	}
	for name, c := range map[string]net.Conn{"first": first, "second": second} {
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatalf("%s connection delivered data", name)
		} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
			t.Fatalf("%s connection stayed open after Close: %v", name, err)
		}
	}
	if n := len(f.tcpSlots); n != 0 {
		t.Fatalf("tcp slots in use after Close: %d", n)
	}
}

func TestDNSForwarderSlowDialCountsAgainstDeadline(t *testing.T) {
	const (
		deadline  = 600 * time.Millisecond
		dialDelay = 400 * time.Millisecond
	)
	// The upstream reads the query and then stays silent and open.
	up := startTCPUpstream(t, func(c net.Conn) {
		readDNSFrame(c)
		io.Copy(io.Discard, c)
	})
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		select {
		case <-time.After(dialDelay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		var d net.Dialer
		return d.DialContext(ctx, network, address)
	}
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, tcpDeadline: deadline, dial: dial})
	defer f.Close()

	start := time.Now()
	c := dnsTCPConn(t, f, "slow")
	c.SetReadDeadline(time.Now().Add(deadline + 2*time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent connection delivered data")
	}
	elapsed := time.Since(start)
	// Without the shared deadline the copy phase would start a fresh budget
	// after the dial and end at dialDelay+deadline.
	if elapsed < deadline-50*time.Millisecond {
		t.Fatalf("connection ended after %v, before the %v deadline", elapsed, deadline)
	}
	if elapsed > deadline+dialDelay/2 {
		t.Fatalf("connection ended after %v: the dial time did not count against the %v deadline", elapsed, deadline)
	}
	requireTCPSlotFreeAfterDial(t, f)
}

// requireTCPSlotFreeAfterDial waits until the one TCP slot of f is free.
func requireTCPSlotFreeAfterDial(t *testing.T, f *DNSForwarder) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.tcpSlots) != 0 {
		if time.Now().After(deadline) {
			t.Fatal("tcp slot was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDNSForwarderFailedDialReleasesSlot(t *testing.T) {
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		return nil, errors.New("dial refused")
	}
	f := startTestDNS(t, "127.0.0.1:1", slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, dial: dial})
	defer f.Close()

	c := dnsTCPConn(t, f, "refused")
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	// The forwarder closes with the query unread, so the guest sees a reset or EOF.
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("guest connection stayed open after a failed dial: %v", err)
	}
	requireTCPSlotFreeAfterDial(t, f)
}

// requireClosedWithin fails unless done closes within d.
func requireClosedWithin(t *testing.T, what string, done <-chan struct{}, d time.Duration) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s did not finish within %v", what, d)
	}
}

func TestDNSForwarderCloseEndsSilentUpstreamConn(t *testing.T) {
	// The default 30-second deadline stays in place: Close must end the
	// connection long before it.
	upstreamEOF := make(chan struct{})
	up := startTCPUpstream(t, func(c net.Conn) {
		readDNSFrame(c)
		// Silent upstream: wait for the forwarder to close its side.
		io.Copy(io.Discard, c)
		close(upstreamEOF)
	})
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	c := dnsTCPConn(t, f, "silent")
	// Let the query reach the upstream, so the handler is in the copy phase.
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	requireClosedWithin(t, "Close", closeInBackground(f), 3*time.Second)
	t.Logf("Close returned after %v", time.Since(start))
	requireClosedWithin(t, "upstream EOF", upstreamEOF, 2*time.Second)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("guest connection stayed open after Close: %v", err)
	}
	if n := len(f.tcpSlots); n != 0 {
		t.Fatalf("tcp slots in use after Close: %d", n)
	}
}

func TestDNSForwarderCloseEndsBlockedDial(t *testing.T) {
	dialing := make(chan struct{})
	var once sync.Once
	// The dial blocks until its context ends, like a connect to an address
	// that never answers.
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		once.Do(func() { close(dialing) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f := startTestDNS(t, "127.0.0.1:1", slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, dial: dial})
	defer f.Close()

	c := dnsTCPConn(t, f, "blocked")
	requireClosedWithin(t, "dial start", dialing, 5*time.Second)
	requireClosedWithin(t, "Close", closeInBackground(f), 3*time.Second)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("guest connection stayed open after Close: %v", err)
	}
	if n := len(f.tcpSlots); n != 0 {
		t.Fatalf("tcp slots in use after Close: %d", n)
	}
}

func TestDNSForwarderCloseDuringDialEndsUpstreamConn(t *testing.T) {
	// The dial finishes while Close runs: the upstream connection that it
	// returns must still be closed.
	upstreamEOF := make(chan struct{})
	up := startTCPUpstream(t, func(c net.Conn) {
		io.Copy(io.Discard, c)
		close(upstreamEOF)
	})
	dialing := make(chan struct{})
	finish := make(chan struct{})
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		close(dialing)
		<-finish
		// Ignore ctx on purpose: a dial can succeed just as Close cancels it.
		var d net.Dialer
		return d.DialContext(context.Background(), network, address)
	}
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, dial: dial})
	defer f.Close()

	c := dnsTCPConn(t, f, "late")
	requireClosedWithin(t, "dial start", dialing, 5*time.Second)
	done := closeInBackground(f)
	// Wait until Close has cancelled the forwarder context, then let the dial
	// return a live connection.
	deadline := time.Now().Add(5 * time.Second)
	for f.ctx.Err() == nil {
		if time.Now().After(deadline) {
			t.Fatal("Close did not cancel the forwarder context")
		}
		time.Sleep(time.Millisecond)
	}
	close(finish)
	requireClosedWithin(t, "Close", done, 3*time.Second)
	requireClosedWithin(t, "upstream EOF", upstreamEOF, 2*time.Second)
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("guest connection stayed open after Close: %v", err)
	}
}

// syncBuffer is a log sink that tests read while the forwarder writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// socketErrorRecords returns the retry log records of one transport.
func (b *syncBuffer) socketErrorRecords(transport DNSTransport) []map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for _, line := range strings.Split(b.buf.String(), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		if rec["level"] == "ERROR" && rec["transport"] == string(transport) {
			out = append(out, rec)
		}
	}
	return out
}

// waitForSocketError waits until a retry record of the transport satisfies
// match (nil matches every record).
func waitForSocketError(t *testing.T, logs *syncBuffer, transport DNSTransport, timeout time.Duration, match func(map[string]any) bool) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		for _, rec := range logs.socketErrorRecords(transport) {
			if match == nil || match(rec) {
				return rec
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("no matching %s retry record within %v: %v", transport, timeout, logs.socketErrorRecords(transport))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestDNSForwarderTCPSurvivesAcceptError(t *testing.T) {
	up := startGatedUpstream(t)
	up.open()
	logs := &syncBuffer{}
	f := startTestDNS(t, up.addr, slog.New(slog.NewJSONHandler(logs, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	// An expired deadline makes Accept fail with a timeout error, like a
	// transient accept failure; the listener itself stays open.
	if err := f.tcp.SetDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := waitForSocketError(t, logs, DNSTransportTCP, 5*time.Second, nil)
	if rec["error"] == nil || rec["retry_in"] == nil {
		t.Fatalf("retry record lacks error or retry_in: %v", rec)
	}
	if err := f.tcp.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	c := dnsTCPConn(t, f, "again")
	requireDNSAnswer(t, c, "again")
}

func TestDNSForwarderUDPSurvivesReadError(t *testing.T) {
	up := startGatedUpstream(t)
	up.open()
	logs := &syncBuffer{}
	f := startTestDNS(t, up.addr, slog.New(slog.NewJSONHandler(logs, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	if err := f.udp.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	rec := waitForSocketError(t, logs, DNSTransportUDP, 5*time.Second, nil)
	if rec["error"] == nil || rec["retry_in"] == nil {
		t.Fatalf("retry record lacks error or retry_in: %v", rec)
	}
	if err := f.udp.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}

	// The loop answers again once its back-off wait ends.
	c := dnsUDPQuery(t, f, "again")
	buf := make([]byte, 64)
	if n, err := c.Read(buf); err != nil || string(buf[:n]) != "ok:again" {
		t.Fatalf("udp answer after the error %q %v", buf[:n], err)
	}
}

func TestDNSForwarderCloseDuringBackoff(t *testing.T) {
	up := startGatedUpstream(t)
	up.open()
	logs := &syncBuffer{}
	f := startTestDNS(t, up.addr, slog.New(slog.NewJSONHandler(logs, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	// Both loops fail on every call, so their back-off reaches its maximum.
	f.tcp.SetDeadline(time.Now())
	f.udp.SetReadDeadline(time.Now())
	atCap := func(rec map[string]any) bool {
		d, _ := rec["retry_in"].(float64)
		return time.Duration(d) == dnsServeBackoffMax
	}
	waitForSocketError(t, logs, DNSTransportTCP, 10*time.Second, atCap)
	waitForSocketError(t, logs, DNSTransportUDP, 10*time.Second, atCap)

	// Both loops now sleep for about a second. Close must not wait for it.
	start := time.Now()
	requireClosedWithin(t, "Close during back-off", closeInBackground(f), 2*time.Second)
	if elapsed := time.Since(start); elapsed > dnsServeBackoffMax/2 {
		t.Fatalf("Close waited %v for a loop in back-off", elapsed)
	}
	// No retry record after Close, even after a full back-off period.
	n := len(logs.socketErrorRecords(DNSTransportTCP)) + len(logs.socketErrorRecords(DNSTransportUDP))
	time.Sleep(dnsServeBackoffMax + 200*time.Millisecond)
	if m := len(logs.socketErrorRecords(DNSTransportTCP)) + len(logs.socketErrorRecords(DNSTransportUDP)); m != n {
		t.Fatalf("retry records after Close: %d before, %d after", n, m)
	}
}
