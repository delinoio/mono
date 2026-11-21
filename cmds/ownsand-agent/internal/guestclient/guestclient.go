// SPDX-License-Identifier: Apache-2.0

// Package guestclient connects the agent to the guest helper of one
// sandbox: Connect GuestService over HTTP/2 without TLS on the hypervisor
// socket that the runtime dials.
package guestclient

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// Dialer connects to the guest helper.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client is the GuestService client of one sandbox.
type Client struct {
	transport *http.Transport
	svc       ownsandv1connect.GuestServiceClient
}

// New returns a client that dials with dial.
func New(dial Dialer) *Client {
	var protocols http.Protocols
	protocols.SetUnencryptedHTTP2(true)
	transport := &http.Transport{
		Protocols: &protocols,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dial(ctx)
		},
		IdleConnTimeout: 5 * time.Minute,
	}
	hc := &http.Client{Transport: transport}
	return &Client{
		transport: transport,
		svc: ownsandv1connect.NewGuestServiceClient(hc, "http://ownsand-guest",
			connect.WithReadMaxBytes(datapath.MaxFrameSize+(64<<10)),
			connect.WithAcceptCompression("gzip", nil, nil),
		),
	}
}

// CheckReady asks the guest helper once.
func (c *Client) CheckReady(ctx context.Context) (*ownsandv1.CheckReadyResponse, error) {
	resp, err := c.svc.CheckReady(ctx, connect.NewRequest(&ownsandv1.CheckReadyRequest{}))
	if err != nil {
		return nil, err
	}
	return resp.Msg, nil
}

// WaitReady polls CheckReady until it succeeds or ctx ends.
func (c *Client) WaitReady(ctx context.Context, interval time.Duration) (*ownsandv1.CheckReadyResponse, error) {
	var lastErr error
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		resp, err := c.CheckReady(attempt)
		cancel()
		if err == nil {
			return resp, nil
		}
		lastErr = err
		c.transport.CloseIdleConnections()
		select {
		case <-ctx.Done():
			return nil, errors.Join(ctx.Err(), lastErr)
		case <-time.After(interval):
		}
	}
}

// SetClock sets the guest clock to t.
func (c *Client) SetClock(ctx context.Context, t time.Time) error {
	_, err := c.svc.SetClock(ctx, connect.NewRequest(&ownsandv1.SetClockRequest{Now: timestamppb.New(t)}))
	return err
}

// Serve opens one Serve stream. The agent opens one stream per operation.
// The stream ends when ctx ends.
func (c *Client) Serve(ctx context.Context) *connect.BidiStreamForClient[ownsandv1.ServeRequest, ownsandv1.ServeResponse] {
	s := c.svc.Serve(ctx)
	CloseOnDone(ctx, s)
	return s
}

// requestCloser is the request side of a client stream.
type requestCloser interface {
	CloseRequest() error
}

// CloseOnDone closes the request side of a bidirectional client stream
// when ctx ends.
//
// Workaround: the HTTP/2 client transport of net/http watches the request
// context only after the request body ends, so a canceled context does not
// unblock Receive while the request body of a bidirectional stream is still
// open. Closing the request body lets the transport observe the canceled
// context and abort the stream. Remove this when net/http aborts an HTTP/2
// stream on context cancellation while the request body is open.
func CloseOnDone(ctx context.Context, s requestCloser) {
	context.AfterFunc(ctx, func() { _ = s.CloseRequest() })
}

// Close closes idle connections.
func (c *Client) Close() {
	c.transport.CloseIdleConnections()
}
