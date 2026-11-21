// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sync"
	"testing"
	"time"
)

const testTimeout = 5 * time.Second

// fakeTap is a TAP device double: frames pushed with inject come out of
// Read, written frames are collected on written, and failRead makes the
// next Read fail.
type fakeTap struct {
	in      chan []byte
	failC   chan error
	written chan []byte
	closed  chan struct{}
	once    sync.Once
}

func newFakeTap() *fakeTap {
	return &fakeTap{in: make(chan []byte, 8), failC: make(chan error, 1), written: make(chan []byte, 8), closed: make(chan struct{})}
}

func (f *fakeTap) Read(p []byte) (int, error) {
	select {
	case b := <-f.in:
		return copy(p, b), nil
	case err := <-f.failC:
		return 0, err
	case <-f.closed:
		return 0, net.ErrClosed
	}
}

func (f *fakeTap) Write(p []byte) (int, error) {
	f.written <- append([]byte(nil), p...)
	return len(p), nil
}

func (f *fakeTap) Close() error {
	f.once.Do(func() { close(f.closed) })
	return nil
}

func (f *fakeTap) isClosed() bool {
	select {
	case <-f.closed:
		return true
	default:
		return false
	}
}

// fakeListener hands out the connections pushed with offer and fails
// Accept with the error pushed with failAccept or after Close.
type fakeListener struct {
	conns  chan net.Conn
	failC  chan error
	closed chan struct{}
	once   sync.Once
}

func newFakeListener() *fakeListener {
	return &fakeListener{conns: make(chan net.Conn, 4), failC: make(chan error, 1), closed: make(chan struct{})}
}

func (l *fakeListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case err := <-l.failC:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *fakeListener) Addr() net.Addr { return &net.TCPAddr{} }

func (l *fakeListener) isClosed() bool {
	select {
	case <-l.closed:
		return true
	default:
		return false
	}
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// connect makes the listener accept a new agent connection and returns the
// agent side of it.
func connect(l *fakeListener) net.Conn {
	agent, guest := net.Pipe()
	l.conns <- guest
	return agent
}

// waitConnected waits until the tunnel has registered an agent connection;
// the tunnel drops TAP frames before that.
func waitConnected(t *testing.T, tun *tunnel) {
	t.Helper()
	waitFor(t, "agent connection", func() bool {
		tun.mu.Lock()
		defer tun.mu.Unlock()
		return tun.conn != nil
	})
}

func frame(payload []byte) []byte {
	b := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(b, uint32(len(payload)))
	copy(b[4:], payload)
	return b
}

func readFrame(t *testing.T, c net.Conn) []byte {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(testTimeout))
	var hdr [4]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		t.Fatalf("read frame header: %v", err)
	}
	payload := make([]byte, binary.BigEndian.Uint32(hdr[:]))
	if _, err := io.ReadFull(c, payload); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	return payload
}

// expectClosed fails the test unless the peer observes that c was closed.
func expectClosed(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(testTimeout))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("connection was not closed: %v", err)
	}
}

func expectFatal(t *testing.T, tun *tunnel) error {
	t.Helper()
	select {
	case err := <-tun.fatal:
		return err
	case <-time.After(testTimeout):
		t.Fatal("no terminal tunnel error")
		return nil
	}
}

func expectNoFatal(t *testing.T, tun *tunnel) {
	t.Helper()
	select {
	case err := <-tun.fatal:
		t.Fatalf("unexpected terminal tunnel error: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(testTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTunnelTapReadErrorIsFatalAndClosesEverything(t *testing.T) {
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	tun.start()
	agent := connect(l)
	defer agent.Close()
	waitConnected(t, tun)
	tap.in <- []byte("ping")
	if got := readFrame(t, agent); string(got) != "ping" {
		t.Fatalf("frame %q", got)
	}

	readErr := errors.New("tap gone")
	tap.failC <- readErr
	if err := expectFatal(t, tun); !errors.Is(err, readErr) {
		t.Fatalf("fatal error %v, want it to wrap %v", err, readErr)
	}
	expectClosed(t, agent)
	waitFor(t, "listener close", l.isClosed)
	waitFor(t, "TAP close", tap.isClosed)
}

func TestTunnelAcceptErrorIsFatalAndClosesEverything(t *testing.T) {
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	tun.start()
	agent := connect(l)
	defer agent.Close()
	agent.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := agent.Write(frame([]byte("hello"))); err != nil {
		t.Fatal(err)
	}
	if got := <-tap.written; string(got) != "hello" {
		t.Fatalf("TAP frame %q", got)
	}

	acceptErr := errors.New("vsock listener failed")
	l.failC <- acceptErr
	if err := expectFatal(t, tun); !errors.Is(err, acceptErr) {
		t.Fatalf("fatal error %v, want it to wrap %v", err, acceptErr)
	}
	expectClosed(t, agent)
	waitFor(t, "TAP close", tap.isClosed)
	waitFor(t, "listener close", l.isClosed)
}

func TestTunnelPeerDropIsNotFatalAndReplacementWorks(t *testing.T) {
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	tun.start()
	defer tun.fail(errors.New("test cleanup"))

	first := connect(l)
	waitConnected(t, tun)
	tun.mu.Lock()
	firstGuest := tun.conn
	tun.mu.Unlock()
	tap.in <- []byte("one")
	if got := readFrame(t, first); string(got) != "one" {
		t.Fatalf("first frame %q", got)
	}
	first.Close()
	expectNoFatal(t, tun)
	if tap.isClosed() || l.isClosed() {
		t.Fatal("dropping the peer closed the TAP device or the listener")
	}

	second := connect(l)
	defer second.Close()
	// The dropped connection clears itself once its reader ends and the
	// replacement then becomes current; wait until the current one is the
	// replacement before sending, because frames for a closed connection
	// are discarded.
	waitFor(t, "replacement connection", func() bool {
		tun.mu.Lock()
		defer tun.mu.Unlock()
		return tun.conn != nil && tun.conn != firstGuest
	})
	tap.in <- []byte("two")
	if got := readFrame(t, second); string(got) != "two" {
		t.Fatalf("replacement frame %q", got)
	}
	second.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := second.Write(frame([]byte("back"))); err != nil {
		t.Fatal(err)
	}
	if got := <-tap.written; !bytes.Equal(got, []byte("back")) {
		t.Fatalf("TAP frame %q", got)
	}
	expectNoFatal(t, tun)
}

func TestTunnelFirstTerminalErrorWins(t *testing.T) {
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	first, second := errors.New("first"), errors.New("second")
	tun.fail(first)
	tun.fail(second) // must neither block nor replace the first error
	if err := expectFatal(t, tun); !errors.Is(err, first) {
		t.Fatalf("fatal error %v, want %v", err, first)
	}
	expectNoFatal(t, tun)
}

func TestTunnelWorkersStopAfterFailure(t *testing.T) {
	// One worker failing closes the listener and the TAP device, so the
	// other worker's own error must not replace the first one.
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	tun.start()
	readErr := errors.New("tap gone")
	tap.failC <- readErr
	if err := expectFatal(t, tun); !errors.Is(err, readErr) {
		t.Fatalf("fatal error %v, want %v", err, readErr)
	}
	waitFor(t, "listener close", l.isClosed)
	expectNoFatal(t, tun)
}

func TestParseTunnelSpec(t *testing.T) {
	spec, ok, err := parseTunnelSpec("console=ttyS0 root=/dev/sda rw ownsand.tunnel=1025,172.30.0.6/30,172.30.0.5 panic=1")
	if !ok || err != nil {
		t.Fatalf("parse: %v %v", ok, err)
	}
	if spec.port != 1025 || spec.guest != netip.MustParsePrefix("172.30.0.6/30") || spec.gateway != netip.MustParseAddr("172.30.0.5") {
		t.Fatalf("spec %+v", spec)
	}
	if _, ok, err := parseTunnelSpec("console=hvc0 root=/dev/vda"); ok || err != nil {
		t.Fatalf("no parameter: %v %v", ok, err)
	}
	for _, bad := range []string{
		"ownsand.tunnel=1025",
		"ownsand.tunnel=0,172.30.0.6/30,172.30.0.5",
		"ownsand.tunnel=1025,fd00::2/64,172.30.0.5",
		"ownsand.tunnel=1025,172.30.0.6/30,nope",
	} {
		if _, ok, err := parseTunnelSpec(bad); !ok || err == nil {
			t.Errorf("%q: ok %v err %v", bad, ok, err)
		}
	}
}
