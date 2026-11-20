// Package controlplane holds the agent's clients of the control plane:
// the HTTP/2 transport, enrollment, the device session credential
// (CreateChallenge and Authenticate) and ConfirmRemoval.
package controlplane

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"connectrpc.com/connect"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const (
	// AuthenticateLabel prefixes the nonce that Authenticate signs.
	AuthenticateLabel = "ownsand-authenticate"
	// ConfirmRemovalLabel prefixes the nonce that ConfirmRemoval signs.
	ConfirmRemovalLabel = "ownsand-confirm-removal"
)

// NewHTTPClient returns an HTTP client that speaks HTTP/2 to origin: over
// TLS for https origins and without TLS (h2c) for http origins, which local
// development and tests use. The agent channel and the relay connection are
// bidirectional streams and need HTTP/2 from end to end. The client has no
// overall timeout, because those streams are long-lived; unary calls use
// context deadlines.
func NewHTTPClient(origin string) (*http.Client, error) {
	u, err := url.Parse(origin)
	if err != nil {
		return nil, fmt.Errorf("parse control plane origin: %w", err)
	}
	t := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	var p http.Protocols
	switch u.Scheme {
	case "https":
		p.SetHTTP2(true)
	case "http":
		p.SetUnencryptedHTTP2(true)
	default:
		return nil, fmt.Errorf("unsupported control plane origin scheme %q", u.Scheme)
	}
	t.Protocols = &p
	// Ping idle HTTP/2 connections so a dead path is detected.
	t.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 15 * time.Second}
	return &http.Client{Transport: t}, nil
}

// Client is the agent's AgentService client.
type Client struct {
	Origin string
	HTTP   *http.Client
	Agent  ownsandv1connect.AgentServiceClient
	Relay  ownsandv1connect.RelayAgentServiceClient
}

// New returns a Client for origin.
func New(origin string) (*Client, error) {
	hc, err := NewHTTPClient(origin)
	if err != nil {
		return nil, err
	}
	return &Client{
		Origin: origin,
		HTTP:   hc,
		Agent:  ownsandv1connect.NewAgentServiceClient(hc, origin),
		Relay:  ownsandv1connect.NewRelayAgentServiceClient(hc, origin),
	}, nil
}

// AuthResult is the outcome of an authentication.
type AuthResult int

const (
	// AuthCredential: the agent got a device session credential.
	AuthCredential AuthResult = iota + 1
	// AuthRemoved: the control plane returned the removal notice.
	AuthRemoved
	// AuthUnknownDevice: the control plane does not know the device.
	AuthUnknownDevice
)

// Session is a device session credential and its expiry.
type Session struct {
	// Credential is the osd_ credential. Secret: never log.
	Credential string
	ExpireTime time.Time
}

// Authenticate gets a new device session credential with CreateChallenge and
// Authenticate. An unknown device is reported as AuthUnknownDevice; the
// control plane answers it with not_found.
func (c *Client) Authenticate(ctx context.Context, deviceID string, key ed25519.PrivateKey, epoch string) (AuthResult, *Session, error) {
	nonce, err := c.challenge(ctx, deviceID)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return AuthUnknownDevice, nil, nil
		}
		return 0, nil, err
	}
	res, err := c.Agent.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
		DeviceId:   deviceID,
		Nonce:      nonce,
		Signature:  Sign(key, AuthenticateLabel, nonce),
		StateEpoch: epoch,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return AuthUnknownDevice, nil, nil
		}
		return 0, nil, err
	}
	switch r := res.Msg.GetResult().(type) {
	case *ownsandv1.AuthenticateResponse_RemovalNotice:
		return AuthRemoved, nil, nil
	case *ownsandv1.AuthenticateResponse_DeviceSessionCredential:
		if r.DeviceSessionCredential == "" {
			return 0, nil, errors.New("authenticate returned an empty credential")
		}
		return AuthCredential, &Session{Credential: r.DeviceSessionCredential, ExpireTime: res.Msg.GetExpireTime().AsTime()}, nil
	default:
		return 0, nil, errors.New("authenticate returned no result")
	}
}

// ConfirmRemoval confirms the local clean-up. An unknown device counts as
// confirmed: an earlier confirmation succeeded and its response was lost.
func (c *Client) ConfirmRemoval(ctx context.Context, deviceID string, key ed25519.PrivateKey) error {
	nonce, err := c.challenge(ctx, deviceID)
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil
		}
		return err
	}
	_, err = c.Agent.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{
		DeviceId:  deviceID,
		Nonce:     nonce,
		Signature: Sign(key, ConfirmRemovalLabel, nonce),
	}))
	if err != nil && connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	return err
}

func (c *Client) challenge(ctx context.Context, deviceID string) ([]byte, error) {
	res, err := c.Agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: deviceID}))
	if err != nil {
		return nil, err
	}
	if len(res.Msg.GetNonce()) == 0 {
		return nil, errors.New("create challenge returned an empty nonce")
	}
	return res.Msg.GetNonce(), nil
}

// Sign signs label || nonce with the device key.
func Sign(key ed25519.PrivateKey, label string, nonce []byte) []byte {
	msg := append([]byte(label), nonce...)
	return ed25519.Sign(key, msg)
}

// BearerHeader returns the Authorization header value of a credential.
func BearerHeader(credential string) string { return "Bearer " + credential }
