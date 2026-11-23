// SPDX-License-Identifier: FSL-1.1-ALv2

package guestclient_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// clockGuest is a GuestService stub that answers SetClock with setClock.
type clockGuest struct {
	ownsandv1connect.UnimplementedGuestServiceHandler
	setClock func() error
}

func (g *clockGuest) SetClock(context.Context, *connect.Request[ownsandv1.SetClockRequest]) (*connect.Response[ownsandv1.SetClockResponse], error) {
	if err := g.setClock(); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.SetClockResponse{}), nil
}

// observedConn reports its first Close on closed.
type observedConn struct {
	net.Conn
	once   sync.Once
	closed chan<- struct{}
}

func (c *observedConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// serveClockGuest serves guest on a loopback listener over HTTP/2 without TLS
// and returns a dialer whose connection reports its Close on the returned
// channel.
func serveClockGuest(t *testing.T, guest *clockGuest) (guestclient.Dialer, <-chan struct{}) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.NewGuestServiceHandler(guest))
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	srv := &http.Server{Handler: mux, Protocols: &protocols}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() { _ = srv.Close() })

	closed := make(chan struct{})
	dial := func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", lis.Addr().String())
		if err != nil {
			return nil, err
		}
		return &observedConn{Conn: conn, closed: closed}, nil
	}
	return dial, closed
}

// requireClosed waits until the dialed connection is closed. The wait is
// condition-based; the timeout only bounds a failing run.
func requireClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(30 * time.Second):
		t.Fatal("the guest connection was not closed after SetClockOnce returned")
	}
}

// TestSetClockOnceClosesTheConnectionOnSuccess checks that the one-use client
// does not leave its idle connection open after a successful call.
func TestSetClockOnceClosesTheConnectionOnSuccess(t *testing.T) {
	dial, closed := serveClockGuest(t, &clockGuest{setClock: func() error { return nil }})
	if err := guestclient.SetClockOnce(context.Background(), dial, time.Unix(1, 0)); err != nil {
		t.Fatalf("SetClockOnce: %v", err)
	}
	requireClosed(t, closed)
}

// TestSetClockOnceClosesTheConnectionOnFailure checks that a failed call
// reaches the caller and still closes the connection of the one-use client.
func TestSetClockOnceClosesTheConnectionOnFailure(t *testing.T) {
	dial, closed := serveClockGuest(t, &clockGuest{setClock: func() error {
		return connect.NewError(connect.CodeInternal, errors.New("clock refused"))
	}})
	err := guestclient.SetClockOnce(context.Background(), dial, time.Unix(1, 0))
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("SetClockOnce error code %v, want %v", connect.CodeOf(err), connect.CodeInternal)
	}
	requireClosed(t, closed)
}

// TestSetClockOnceReportsDialFailure checks that a failed dial still
// surfaces as a *DialError.
func TestSetClockOnceReportsDialFailure(t *testing.T) {
	err := guestclient.SetClockOnce(context.Background(), failingDialer, time.Unix(1, 0))
	var dialErr *guestclient.DialError
	if !errors.As(err, &dialErr) || !errors.Is(err, errNoGuest) {
		t.Fatalf("SetClockOnce error %v, want a *DialError that wraps the dial error", err)
	}
}
