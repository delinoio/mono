// SPDX-License-Identifier: FSL-1.1-ALv2

package netpolicy

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

// udpShutdownBound is a failure guard, not an expectation: Close must return
// far sooner. It is below the 5-second upstream timeout, so a Close that only
// ends with that timeout fails.
const udpShutdownBound = 4 * time.Second

// startSilentUDPUpstream starts a loopback UDP resolver that reads queries and
// never replies. The returned channel is closed when the first query arrived.
func startSilentUDPUpstream(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	conn, err := net.ListenUDP("udp", net.UDPAddrFromAddrPort(netip.MustParseAddrPort("127.0.0.1:0")))
	if err != nil {
		t.Fatalf("listen udp upstream: %v", err)
	}
	received := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		var once sync.Once
		buf := make([]byte, 512)
		for {
			if _, _, err := conn.ReadFromUDPAddrPort(buf); err != nil {
				return
			}
			once.Do(func() { close(received) })
		}
	}()
	t.Cleanup(func() {
		conn.Close()
		<-done
	})
	return conn.LocalAddr().String(), received
}

// closeTrackedConn is a net.Conn whose reads block until it is closed.
type closeTrackedConn struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newCloseTrackedConn() *closeTrackedConn {
	return &closeTrackedConn{closed: make(chan struct{})}
}

func (c *closeTrackedConn) Read([]byte) (int, error) {
	<-c.closed
	return 0, net.ErrClosed
}

func (c *closeTrackedConn) Write(b []byte) (int, error) { return len(b), nil }

func (c *closeTrackedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *closeTrackedConn) LocalAddr() net.Addr              { return &net.UDPAddr{} }
func (c *closeTrackedConn) RemoteAddr() net.Addr             { return &net.UDPAddr{} }
func (c *closeTrackedConn) SetDeadline(time.Time) error      { return nil }
func (c *closeTrackedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *closeTrackedConn) SetWriteDeadline(time.Time) error { return nil }

func TestDNSForwarderCloseEndsSilentUDPUpstream(t *testing.T) {
	up, received := startSilentUDPUpstream(t)
	f := startTestDNS(t, up, slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1})
	defer f.Close()

	dnsUDPQuery(t, f, "silent")
	// The query reached the upstream, so the worker waits for a reply that
	// never comes.
	requireClosedWithin(t, "upstream query", received, 5*time.Second)
	start := time.Now()
	requireClosedWithin(t, "Close", closeInBackground(f), udpShutdownBound)
	t.Logf("Close returned after %v", time.Since(start))
	if n := len(f.udpSlots); n != 0 {
		t.Fatalf("udp slots in use after Close: %d", n)
	}
}

func TestDNSForwarderCloseEndsBlockedUDPDial(t *testing.T) {
	dialing := make(chan struct{})
	var once sync.Once
	// The dial blocks until its context ends.
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		once.Do(func() { close(dialing) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	f := startTestDNS(t, "127.0.0.1:1", slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, dialUDP: dial})
	defer f.Close()

	dnsUDPQuery(t, f, "blocked")
	requireClosedWithin(t, "dial start", dialing, 5*time.Second)
	requireClosedWithin(t, "Close", closeInBackground(f), udpShutdownBound)
	if n := len(f.udpSlots); n != 0 {
		t.Fatalf("udp slots in use after Close: %d", n)
	}
}

func TestDNSForwarderCloseDuringUDPDialClosesSocket(t *testing.T) {
	type exitPoint int
	const (
		beforeClose exitPoint = iota
		duringClose
		afterSocketClose
	)
	for _, tc := range []struct {
		name string
		exit exitPoint
	}{
		{"cleanup before Close", beforeClose},
		{"cleanup during Close", duringClose},
		{"live socket returned after cancellation", afterSocketClose},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sock := newCloseTrackedConn()
			dialing := make(chan struct{})
			finish := make(chan struct{})
			releaseDial := sync.OnceFunc(func() { close(finish) })
			dial := func(context.Context, string, string) (net.Conn, error) {
				close(dialing)
				<-finish
				// A dial can succeed just as Close cancels its context.
				return sock, nil
			}
			f := startTestDNS(t, "127.0.0.1:1", slog.New(slog.NewJSONHandler(io.Discard, nil)), dnsLimits{maxUDPQueries: 1, maxTCPConns: 1, dialUDP: dial})
			// A second Close can return while the first still waits for workers.
			startClose := sync.OnceValue(func() <-chan struct{} { return closeInBackground(f) })
			t.Cleanup(func() {
				// Close waits for the dial. Release it even if it has yet to start.
				releaseDial()
				// Unblock the fixture read if the forwarder leaves it open.
				sock.Close()
				requireClosedWithin(t, "Close during cleanup", startClose(), udpShutdownBound)
				if n := len(f.udpSlots); n != 0 {
					t.Fatalf("udp slots in use after cleanup: %d", n)
				}
			})

			dnsUDPQuery(t, f, "late")
			requireClosedWithin(t, "dial start", dialing, 5*time.Second)
			if tc.exit == beforeClose {
				return
			}
			done := startClose()
			// The dial returns a live socket only after Close cancels the forwarder.
			requireClosedWithin(t, "forwarder cancellation", f.ctx.Done(), 5*time.Second)
			if tc.exit == duringClose {
				return
			}
			releaseDial()
			requireClosedWithin(t, "Close", done, udpShutdownBound)
			requireClosedWithin(t, "socket close", sock.closed, 2*time.Second)
			if n := len(f.udpSlots); n != 0 {
				t.Fatalf("udp slots in use after Close: %d", n)
			}
		})
	}
}
