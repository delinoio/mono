// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"crypto/rand"
	"errors"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

// stalledListener accepts connections whose writes stall after limit bytes,
// like a server socket whose peer stopped draining its network path once the
// kernel buffers filled. The test blocks the writes itself instead of filling
// the kernel buffers: how a blocked socket write progresses depends on the
// platform. Linux lets it advance a little at a time, so a write deadline
// often ends a write with partial progress; Windows lets it make none.
type stalledListener struct {
	net.Listener
	limit int
	// blocked counts the writes that stalled.
	blocked atomic.Int64
}

func (l *stalledListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &stalledConn{Conn: c, l: l, left: l.limit, changed: make(chan struct{}), closed: make(chan struct{})}, nil
}

// stalledConn passes its writes until they reach limit bytes and then blocks
// every later write without progress until the write deadline passes or the
// connection closes, as a socket write does when the kernel buffers are full.
// The HTTP/2 write byte timeout ends such a write through the write deadline.
type stalledConn struct {
	net.Conn
	l        *stalledListener
	mu       sync.Mutex
	left     int
	deadline time.Time
	// changed is closed and replaced when the write deadline changes, so
	// that a blocked write reads the new deadline.
	changed chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *stalledConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	pass := c.left > 0
	c.left -= len(p)
	c.mu.Unlock()
	if pass {
		return c.Conn.Write(p)
	}
	c.l.blocked.Add(1)
	for {
		c.mu.Lock()
		deadline, changed := c.deadline, c.changed
		c.mu.Unlock()
		var expired <-chan time.Time
		var timer *time.Timer
		if !deadline.IsZero() {
			wait := time.Until(deadline)
			if wait <= 0 {
				return 0, os.ErrDeadlineExceeded
			}
			timer = time.NewTimer(wait)
			expired = timer.C
		}
		select {
		case <-expired:
			return 0, os.ErrDeadlineExceeded
		case <-changed:
			if timer != nil {
				timer.Stop()
			}
		case <-c.closed:
			return 0, net.ErrClosed
		}
	}
}

func (c *stalledConn) setWriteDeadline(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	close(c.changed)
	c.changed = make(chan struct{})
}

func (c *stalledConn) SetWriteDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetWriteDeadline(t)
}

func (c *stalledConn) SetDeadline(t time.Time) error {
	c.setWriteDeadline(t)
	return c.Conn.SetDeadline(t)
}

func (c *stalledConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// sendInFlight reports whether a goroutine is inside the server side of a
// Connect bidirectional stream's Send. The HTTP/2 server finishes a response
// when its handler returns, so no Send may still write to it by then.
func sendInFlight() bool {
	buf := make([]byte, 1<<20)
	for {
		n := runtime.Stack(buf, true)
		if n < len(buf) {
			return strings.Contains(string(buf[:n]), "BidiStream[...]).Send(")
		}
		buf = make([]byte, 2*len(buf))
	}
}

// TestRelayAgentConnectEndsBlockedSendBeforeReturning runs the production
// Connect serving path (RelayAgentWriteInterrupt and serveRelayConn) over a
// real HTTP/2 connection. The agent sends nothing after its first frame, so
// the idle timeout closes the relay connection while the writer's send is
// blocked on the agent. The handler must return only after that send ended:
// returning earlier lets the HTTP/2 server finish the response while the send
// still writes to it (a data race inside net/http).
func TestRelayAgentConnectEndsBlockedSendBeforeReturning(t *testing.T) {
	for _, tc := range []struct {
		name string
		// stallSocket blocks the server's socket writes, as when the
		// client stops draining its network path; otherwise the client
		// reads the socket but never receives the stream, so that the
		// stream's flow control window closes.
		stallSocket bool
	}{
		{name: "flow control window closed", stallSocket: false},
		{name: "socket writes blocked", stallSocket: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := relay.Limits{StreamBuffer: datapath.MinWindowSize, MaxStreamsPerConn: 4, MaxStreamsPerGrant: 4,
				Keepalive: 150 * time.Millisecond, IdleTimeout: 600 * time.Millisecond}
			type result struct {
				err     error
				elapsed time.Duration
				// inFlight is set when a server stream.Send still ran after Run returned.
				inFlight bool
			}
			handled := make(chan result, 1)
			senderDone := make(chan struct{})
			handler := connect.NewBidiStreamHandler(ownsandv1connect.RelayAgentServiceConnectProcedure,
				func(ctx context.Context, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
					start := time.Now()
					conn := relay.NewConn(ctx, "device", 1, limits)
					s, err := conn.Open(ctx, "grant", "sandbox")
					if err != nil {
						return err
					}
					// Queue frames toward the agent until the writer blocks.
					go func() {
						defer close(senderDone)
						// Random data: Connect compresses a repetitive payload to a few
						// bytes, which would never fill the peer's window.
						data := make([]byte, 512<<10)
						_, _ = rand.Read(data)
						chunk := &ownsandv1.WriteFileRequest{Data: data}
						for {
							if err := s.Send(ctx, datapath.MustNewFrame(0, "", chunk)); err != nil {
								return
							}
						}
					}()
					err = serveRelayConn(ctx, conn, stream)
					handled <- result{err: err, elapsed: time.Since(start), inFlight: sendInFlight()}
					return nil
				})
			var protocols http.Protocols
			protocols.SetUnencryptedHTTP2(true)
			// A write interrupt cannot end a socket write that the kernel
			// buffers block; the HTTP/2 write byte timeout does, and the
			// production server sets it (see newServer in cmd/ownsand-api).
			// Here it is longer than the idle timeout, so that the idle
			// timeout closes the relay connection while the socket write
			// is still blocked. In production that happens when a socket
			// that drains slowly keeps restarting the write byte timeout.
			// A shorter one closes the HTTP/2 connection first, and Run
			// then ends with the canceled request context instead.
			srv := &http.Server{Handler: RelayAgentWriteInterrupt(handler), Protocols: &protocols,
				HTTP2: &http.HTTP2Config{WriteByteTimeout: 2 * time.Second}}
			tcpListener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			var listener net.Listener = tcpListener
			var stalled *stalledListener
			if tc.stallSocket {
				stalled = &stalledListener{Listener: tcpListener, limit: 64 << 10}
				listener = stalled
			}
			go func() { _ = srv.Serve(listener) }()
			t.Cleanup(func() { _ = srv.Close() })

			var clientProtocols http.Protocols
			clientProtocols.SetUnencryptedHTTP2(true)
			transport := &http.Transport{Protocols: &clientProtocols}
			t.Cleanup(transport.CloseIdleConnections)
			client := ownsandv1connect.NewRelayAgentServiceClient(&http.Client{Transport: transport}, "http://"+listener.Addr().String())
			cs := client.Connect(context.Background())
			// The first request frame grants the credit for stream 1, so that the
			// server's writer has frames to send. The client sends nothing after it.
			grant := datapath.MustNewFrame(1, "sandbox", &ownsandv1.RelayWindowUpdate{IncrementBytes: datapath.MaxWindowSize})
			if err := cs.Send(&ownsandv1.ConnectRequest{Frame: grant}); err != nil {
				t.Fatal(err)
			}

			select {
			case r := <-handled:
				if !errors.Is(r.err, relay.ErrIdle) {
					t.Fatalf("Run = %v, want %v", r.err, relay.ErrIdle)
				}
				if r.inFlight {
					t.Fatal("the handler returned while a stream.Send was still in flight")
				}
				if r.elapsed > 10*time.Second {
					t.Fatalf("the connection closed after %v", r.elapsed)
				}
				if stalled != nil && stalled.blocked.Load() == 0 {
					t.Fatal("no server socket write blocked")
				}
			case <-time.After(15 * time.Second):
				t.Fatal("the handler did not return for an idle connection with a blocked send")
			}
			select {
			case <-senderDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the stream sender is still running after the connection closed")
			}
		})
	}
}
