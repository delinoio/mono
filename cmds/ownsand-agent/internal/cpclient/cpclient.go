// SPDX-License-Identifier: Apache-2.0

// Package cpclient connects the agent to the control plane: the HTTP/2
// client (TLS h2 for https origins, h2c with prior knowledge for http
// origins in development and tests), the AgentService and RelayAgentService
// clients, and the challenge-signing flows of Authenticate and
// ConfirmRemoval. It never logs credentials.
package cpclient

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// Client holds the control plane clients of one origin.
type Client struct {
	APIURL string
	HTTP   *http.Client
	Agent  ownsandv1connect.AgentServiceClient
	Relay  ownsandv1connect.RelayAgentServiceClient
}

// NewHTTPClient returns an HTTP client that speaks HTTP/2 to apiURL: TLS
// with ALPN for https, and HTTP/2 without TLS (prior knowledge) for http,
// because the bidirectional agent channel and relay connection need HTTP/2
// from end to end.
func NewHTTPClient(apiURL string, tlsConfig *tls.Config) (*http.Client, error) {
	u, err := url.Parse(apiURL)
	if err != nil {
		return nil, fmt.Errorf("cpclient: invalid control plane origin: %w", err)
	}
	var protocols http.Protocols
	switch u.Scheme {
	case "https":
		protocols.SetHTTP2(true)
	case "http":
		protocols.SetUnencryptedHTTP2(true)
	default:
		return nil, fmt.Errorf("cpclient: control plane origin must use https or http, not %q", u.Scheme)
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         dialer.DialContext,
		TLSClientConfig:     tlsConfig,
		TLSHandshakeTimeout: 15 * time.Second,
		IdleConnTimeout:     90 * time.Second,
		Protocols:           &protocols,
		ForceAttemptHTTP2:   true,
	}
	// HTTP/2 health checks detect a dead connection under a long stream.
	transport.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 15 * time.Second}
	return &http.Client{Transport: transport}, nil
}

// New returns the clients of apiURL.
func New(apiURL string, tlsConfig *tls.Config) (*Client, error) {
	hc, err := NewHTTPClient(apiURL, tlsConfig)
	if err != nil {
		return nil, err
	}
	readMax := connect.WithReadMaxBytes(datapath.MaxFrameSize + (64 << 10))
	return &Client{
		APIURL: apiURL,
		HTTP:   hc,
		Agent:  ownsandv1connect.NewAgentServiceClient(hc, apiURL, readMax),
		Relay:  ownsandv1connect.NewRelayAgentServiceClient(hc, apiURL, readMax),
	}, nil
}

// SetBearer sets the Authorization header of a request.
func SetBearer(h http.Header, credential string) {
	h.Set("Authorization", "Bearer "+credential)
}

// Session is a device session credential. Secret: never log Credential.
type Session struct {
	Credential     string
	ExpiresIn      time.Duration
	OrganizationID string
}

// ErrUnknownDevice reports a not_found answer of Authenticate or
// ConfirmRemoval: the control plane does not know the device.
var ErrUnknownDevice = errors.New("cpclient: the control plane does not know this device")

// AuthResult is a session or a removal notice.
type AuthResult struct {
	Session *Session
	Removal *ownsandv1.RemovalNotice
}

func (c *Client) challenge(ctx context.Context, deviceID string) ([]byte, error) {
	resp, err := c.Agent.CreateChallenge(ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: deviceID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrUnknownDevice
		}
		return nil, fmt.Errorf("cpclient: create challenge: %w", err)
	}
	return resp.Msg.GetNonce(), nil
}

// Authenticate gets a new device session credential, or the removal notice.
func (c *Client) Authenticate(ctx context.Context, deviceID string, key *identity.Key, stateEpoch string) (*AuthResult, error) {
	nonce, err := c.challenge(ctx, deviceID)
	if err != nil {
		return nil, err
	}
	resp, err := c.Agent.Authenticate(ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
		DeviceId:   deviceID,
		Nonce:      nonce,
		Signature:  key.Sign(identity.PurposeAuthenticate, nonce),
		StateEpoch: stateEpoch,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrUnknownDevice
		}
		return nil, fmt.Errorf("cpclient: authenticate: %w", err)
	}
	switch r := resp.Msg.GetResult().(type) {
	case *ownsandv1.AuthenticateResponse_Session:
		s := r.Session
		expiresIn := s.GetExpiresIn().AsDuration()
		if s.GetExpiresIn() == nil && s.GetExpireTime() != nil {
			expiresIn = time.Until(s.GetExpireTime().AsTime())
		}
		if s.GetDeviceSessionCredential() == "" {
			return nil, errors.New("cpclient: authenticate returned an empty credential")
		}
		return &AuthResult{Session: &Session{Credential: s.GetDeviceSessionCredential(), ExpiresIn: expiresIn, OrganizationID: s.GetOrganizationId()}}, nil
	case *ownsandv1.AuthenticateResponse_RemovalNotice:
		return &AuthResult{Removal: r.RemovalNotice}, nil
	}
	return nil, errors.New("cpclient: authenticate returned neither a session nor a removal notice")
}

// ConfirmRemoval confirms the local clean-up of a removed device. An
// unknown device gives ErrUnknownDevice, which the caller treats as an
// earlier confirmation whose response was lost.
func (c *Client) ConfirmRemoval(ctx context.Context, deviceID string, key *identity.Key) error {
	nonce, err := c.challenge(ctx, deviceID)
	if err != nil {
		return err
	}
	_, err = c.Agent.ConfirmRemoval(ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{
		DeviceId:  deviceID,
		Nonce:     nonce,
		Signature: key.Sign(identity.PurposeConfirmRemoval, nonce),
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return ErrUnknownDevice
		}
		return fmt.Errorf("cpclient: confirm removal: %w", err)
	}
	return nil
}
