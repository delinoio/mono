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
	"strings"
	"sync"
	"sync/atomic"
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
func connect(t *testing.T, l *fakeListener) net.Conn {
	t.Helper()
	agent, guest := net.Pipe()
	t.Cleanup(func() {
		agent.Close()
		guest.Close()
	})
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
	agent := connect(t, l)
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

// pausedTunnelAccept holds a connection after the listener has accepted it
// but before the tunnel can register it.
type pausedTunnelAccept struct {
	net.Listener
	accepted chan struct{}
	release  chan struct{}
}

func (l *pausedTunnelAccept) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		close(l.accepted)
		<-l.release
	}
	return c, err
}

type tunnelReadCounter struct {
	net.Conn
	reads atomic.Int32
}

func (c *tunnelReadCounter) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.Conn.Read(p)
}

func TestTunnelShutdownClosesPendingAcceptBeforeNotify(t *testing.T) {
	tap, listener := newFakeTap(), newFakeListener()
	l := &pausedTunnelAccept{
		Listener: listener,
		accepted: make(chan struct{}),
		release:  make(chan struct{}),
	}
	release := sync.OnceFunc(func() { close(l.release) })
	t.Cleanup(release)

	agent, guest := net.Pipe()
	t.Cleanup(func() { agent.Close(); guest.Close() })
	pending := &tunnelReadCounter{Conn: guest}
	listener.conns <- pending

	tun := newTunnel(tap, l, testLogger())
	tun.start()
	select {
	case <-l.accepted:
	case <-time.After(testTimeout):
		t.Fatal("listener did not accept the pending connection")
	}

	readErr := errors.New("tap gone")
	tap.failC <- readErr
	waitFor(t, "listener close", listener.isClosed)
	waitFor(t, "TAP close", tap.isClosed)
	select {
	case err := <-tun.fatal:
		t.Fatalf("reported %v before closing the pending connection", err)
	default:
	}

	release()
	if err := expectFatal(t, tun); !errors.Is(err, readErr) {
		t.Fatalf("fatal error %v, want it to wrap %v", err, readErr)
	}
	expectClosed(t, agent)
	if reads := pending.reads.Load(); reads != 0 {
		t.Fatalf("started reading from the pending connection: %d reads", reads)
	}
}

func TestTunnelAcceptErrorIsFatalAndClosesEverything(t *testing.T) {
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	tun.start()
	t.Cleanup(func() { tun.fail(errors.New("test cleanup")) })
	agent := connect(t, l)
	agent.SetWriteDeadline(time.Now().Add(testTimeout))
	if _, err := agent.Write(frame([]byte("hello"))); err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-tap.written:
		if string(got) != "hello" {
			t.Fatalf("TAP frame %q", got)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the TAP frame from the agent")
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
	t.Cleanup(func() { tun.fail(errors.New("test cleanup")) })

	first := connect(t, l)
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

	second := connect(t, l)
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
	select {
	case got := <-tap.written:
		if !bytes.Equal(got, []byte("back")) {
			t.Fatalf("TAP frame %q", got)
		}
	case <-time.After(testTimeout):
		t.Fatal("timed out waiting for the TAP frame from the replacement agent")
	}
	expectNoFatal(t, tun)
}

func TestTunnelFirstTerminalErrorWins(t *testing.T) {
	tap, l := newFakeTap(), newFakeListener()
	tun := newTunnel(tap, l, testLogger())
	tun.start()
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

// setupRecorder builds tunnelDeps whose operations succeed and record their
// calls; a test overrides one operation to inject a failure.
type setupRecorder struct {
	tap          *fakeTap
	listener     *fakeListener
	listenCalls  int
	listenAddr   string
	resolverPath string
	resolverData string
}

func (r *setupRecorder) deps() tunnelDeps {
	return tunnelDeps{
		openTap: func(string) (io.ReadWriteCloser, error) { return r.tap, nil },
		configureInterface: func(string, netip.Prefix, netip.Addr) error {
			return nil
		},
		writeResolver: func(path string, data []byte) error {
			r.resolverPath, r.resolverData = path, string(data)
			return nil
		},
		listen: func(address string) (net.Listener, error) {
			r.listenCalls++
			r.listenAddr = address
			return r.listener, nil
		},
	}
}

func newSetupRecorder() *setupRecorder {
	return &setupRecorder{tap: newFakeTap(), listener: newFakeListener()}
}

func testTunnelSpec() tunnelSpec {
	return tunnelSpec{port: 1025, guest: netip.MustParsePrefix("172.30.0.6/30"), gateway: netip.MustParseAddr("172.30.0.5")}
}

func TestTunnelSetupResolverWriteFailureClosesTapAndSkipsListener(t *testing.T) {
	rec := newSetupRecorder()
	deps := rec.deps()
	writeErr := errors.New("read-only file system")
	deps.writeResolver = func(string, []byte) error { return writeErr }

	fatal, err := startTunnelWith(testTunnelSpec(), deps, testLogger())
	if err == nil {
		t.Fatal("setup succeeded although the resolver write failed")
	}
	if fatal != nil {
		t.Fatal("a failed setup returned a fatal channel")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("error %q does not wrap the write error", err)
	}
	if !strings.Contains(err.Error(), "/etc/resolv.conf") {
		t.Errorf("error %q does not name /etc/resolv.conf", err)
	}
	if !rec.tap.isClosed() {
		t.Error("the TAP device stayed open")
	}
	if rec.listenCalls != 0 {
		t.Errorf("the listener opened %d times", rec.listenCalls)
	}
}

func TestTunnelSetupWritesGatewayAndStartsRelay(t *testing.T) {
	rec := newSetupRecorder()
	fatal, err := startTunnelWith(testTunnelSpec(), rec.deps(), testLogger())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	if fatal == nil {
		t.Fatal("no fatal channel")
	}
	t.Cleanup(func() { rec.listener.Close(); rec.tap.Close() })
	if rec.resolverPath != "/etc/resolv.conf" || rec.resolverData != "nameserver 172.30.0.5\n" {
		t.Errorf("resolver %q: %q", rec.resolverPath, rec.resolverData)
	}
	if rec.listenCalls != 1 || rec.listenAddr != "vsock://:1025" {
		t.Errorf("listen calls %d address %q", rec.listenCalls, rec.listenAddr)
	}
	if rec.tap.isClosed() {
		t.Error("the TAP device closed on a successful setup")
	}
}

func TestTunnelSetupConfigureFailureClosesTapBeforeResolverAndListener(t *testing.T) {
	rec := newSetupRecorder()
	deps := rec.deps()
	deps.configureInterface = func(string, netip.Prefix, netip.Addr) error { return errors.New("no route") }
	resolverWrites := 0
	deps.writeResolver = func(string, []byte) error { resolverWrites++; return nil }

	if _, err := startTunnelWith(testTunnelSpec(), deps, testLogger()); err == nil {
		t.Fatal("setup succeeded although the interface configuration failed")
	}
	if !rec.tap.isClosed() || resolverWrites != 0 || rec.listenCalls != 0 {
		t.Errorf("tap closed %v, resolver writes %d, listen calls %d", rec.tap.isClosed(), resolverWrites, rec.listenCalls)
	}
}
