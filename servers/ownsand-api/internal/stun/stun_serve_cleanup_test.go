// SPDX-License-Identifier: FSL-1.1-ALv2

package stun

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// cleanupPacketConn is a net.PacketConn whose ReadFrom either fails at once with
// readErr or blocks until Close, like a real UDP socket.
type cleanupPacketConn struct {
	readErr error

	closeOnce sync.Once
	closed    chan struct{}
	closes    atomic.Int32
}

func newCleanupPacketConn(readErr error) *cleanupPacketConn {
	return &cleanupPacketConn{readErr: readErr, closed: make(chan struct{})}
}

func (c *cleanupPacketConn) ReadFrom([]byte) (int, net.Addr, error) {
	if c.readErr != nil {
		return 0, nil, c.readErr
	}
	<-c.closed
	return 0, nil, net.ErrClosed
}

func (c *cleanupPacketConn) WriteTo(p []byte, _ net.Addr) (int, error) { return len(p), nil }

func (c *cleanupPacketConn) Close() error {
	c.closes.Add(1)
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *cleanupPacketConn) LocalAddr() net.Addr { return &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)} }

func (c *cleanupPacketConn) SetDeadline(time.Time) error      { return nil }
func (c *cleanupPacketConn) SetReadDeadline(time.Time) error  { return nil }
func (c *cleanupPacketConn) SetWriteDeadline(time.Time) error { return nil }

var _ net.PacketConn = (*cleanupPacketConn)(nil)

// newCleanupServer returns a server whose stats logger reports its exit on the
// returned channel.
func newCleanupServer() (*Server, <-chan struct{}) {
	s := New(slog.New(slog.NewJSONHandler(io.Discard, nil)), 2048, 8192)
	stopped := make(chan struct{})
	s.statsStopped = func() { close(stopped) }
	return s, stopped
}

// assertHelpersStopped checks, right after Serve returned, that the stats
// logger already exited. Serve must wait for it, so no waiting is allowed here.
func assertHelpersStopped(t *testing.T, stopped <-chan struct{}) {
	t.Helper()
	select {
	case <-stopped:
	default:
		t.Fatal("stats logger still running after Serve returned")
	}
}

func TestServeReadErrorReleasesResources(t *testing.T) {
	sentinel := errors.New("socket fault")
	pc := newCleanupPacketConn(sentinel)
	s, stopped := newCleanupServer()
	// The parent context stays active: only the read error ends Serve.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	err := s.Serve(ctx, pc)
	if !errors.Is(err, sentinel) {
		t.Fatalf("Serve did not return the read error: %v", err == nil)
	}
	if got := pc.closes.Load(); got != 1 {
		t.Fatalf("PacketConn closed %d times, want 1", got)
	}
	select {
	case <-pc.closed:
	default:
		t.Fatal("PacketConn not closed after Serve returned")
	}
	assertHelpersStopped(t, stopped)
	if ctx.Err() != nil {
		t.Fatal("Serve must not cancel the parent context")
	}
}

func TestServeCancelReleasesBlockedRead(t *testing.T) {
	pc := newCleanupPacketConn(nil)
	s, stopped := newCleanupServer()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, pc) }()
	// Serve is blocked in ReadFrom until the parent context ends and the
	// closer releases the connection.
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Serve after cancellation returned an error: %v", errors.Is(err, net.ErrClosed))
	}
	if got := pc.closes.Load(); got != 1 {
		t.Fatalf("PacketConn closed %d times, want 1", got)
	}
	assertHelpersStopped(t, stopped)
}
