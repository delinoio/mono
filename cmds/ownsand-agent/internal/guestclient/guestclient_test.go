// SPDX-License-Identifier: FSL-1.1-ALv2

package guestclient_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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
