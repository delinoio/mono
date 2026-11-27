// SPDX-License-Identifier: FSL-1.1-ALv2

package guestclient_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

var errNoGuest = errors.New("no guest")

func failingDialer(context.Context) (net.Conn, error) { return nil, errNoGuest }

// TestDialFailureIsTyped checks that a failed dial reaches the caller of a
// unary call and of a stream as a *DialError that keeps its cause.
func TestDialFailureIsTyped(t *testing.T) {
	c := guestclient.New(failingDialer)
	defer c.Close()

	_, err := c.CheckReady(context.Background())
	var dialErr *guestclient.DialError
	if !errors.As(err, &dialErr) || !errors.Is(err, errNoGuest) {
		t.Fatalf("CheckReady error %v, want a *DialError that wraps the dial error", err)
	}

	s := c.Serve(context.Background())
	defer s.CloseResponse()
	// Connect reports a failed stream as io.EOF from Send and keeps the real
	// error for Receive.
	if err := s.Send(&ownsandv1.ServeRequest{}); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Send error %v", err)
	}
	_, err = s.Receive()
	if !errors.As(err, &dialErr) || !errors.Is(err, errNoGuest) {
		t.Fatalf("Receive error %v, want a *DialError that wraps the dial error", err)
	}
}

// TestWithDialerWrapsTheOriginalDialer checks that WithDialer passes the
// unwrapped dialer of the original client to the wrapper.
func TestWithDialerWrapsTheOriginalDialer(t *testing.T) {
	c := guestclient.New(failingDialer)
	var inner guestclient.Dialer
	w := c.WithDialer(func(d guestclient.Dialer) guestclient.Dialer {
		inner = d
		return d
	})
	defer w.Close()
	if _, err := inner(context.Background()); !errors.Is(err, errNoGuest) {
		t.Fatalf("inner dialer error %v, want the original error", err)
	}
	var dialErr *guestclient.DialError
	_, err := w.CheckReady(context.Background())
	if !errors.As(err, &dialErr) {
		t.Fatalf("CheckReady through the wrapped client: %v, want a *DialError", err)
	}
}

type clockService struct {
	ownsandv1connect.UnimplementedGuestServiceHandler
	now time.Time
	err error
}

func (s clockService) SetClock(_ context.Context, req *connect.Request[ownsandv1.SetClockRequest]) (*connect.Response[ownsandv1.SetClockResponse], error) {
	if !req.Msg.GetNow().AsTime().Equal(s.now) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("unexpected clock value"))
	}
	if s.err != nil {
		return nil, s.err
	}
	return connect.NewResponse(&ownsandv1.SetClockResponse{}), nil
}

func TestSetClockOnceClosesHTTP2Connection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "RPC failure", err: connect.NewError(connect.CodePermissionDenied, errors.New("clock denied"))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			now := time.Now()
			_, handler := ownsandv1connect.NewGuestServiceHandler(clockService{now: now, err: tc.err})
			var protocols http.Protocols
			protocols.SetUnencryptedHTTP2(true)
			closed := make(chan struct{}, 1)
			server := &http.Server{
				Protocols: &protocols,
				Handler:   handler,
				ConnState: func(_ net.Conn, state http.ConnState) {
					if state == http.StateClosed {
						closed <- struct{}{}
					}
				},
			}
			t.Cleanup(func() { _ = server.Close() })
			go func() { _ = server.Serve(listener) }()

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var dialer net.Dialer
			err = guestclient.SetClockOnce(ctx, func(ctx context.Context) (net.Conn, error) {
				return dialer.DialContext(ctx, "tcp", listener.Addr().String())
			}, now)
			if tc.err == nil {
				if err != nil {
					t.Fatal(err)
				}
			} else if connect.CodeOf(err) != connect.CodeOf(tc.err) {
				t.Fatalf("SetClockOnce error = %v, want %v", err, tc.err)
			}
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("HTTP/2 peer did not observe connection closure after SetClockOnce")
			}
		})
	}
}

func TestSetClockOnceClosesLateDial(t *testing.T) {
	conn, peer := net.Pipe()
	t.Cleanup(func() { _ = conn.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	ctx, timeout := context.WithTimeout(t.Context(), 5*time.Second)
	defer timeout()
	callCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- guestclient.SetClockOnce(callCtx, func(context.Context) (net.Conn, error) {
			close(started)
			<-release
			return conn, nil
		}, time.Now())
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("dial did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SetClockOnce error = %v, want context.Canceled", err)
		}
	case <-ctx.Done():
		t.Fatal("SetClockOnce waited for the canceled dial")
	}
	if err := peer.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	var b [1]byte
	if _, err := peer.Read(b[:]); err != io.EOF {
		t.Fatalf("read from late dial peer = %v, want EOF", err)
	}
}
