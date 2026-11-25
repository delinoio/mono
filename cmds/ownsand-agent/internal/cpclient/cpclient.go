// SPDX-License-Identifier: FSL-1.1-ALv2

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
	"net/netip"
	"net/url"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/idna"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/identity"
	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// DefaultAuthTimeout is the deadline of one authentication attempt (the
// challenge and the signed call together) when Client.AuthTimeout is zero.
const DefaultAuthTimeout = 30 * time.Second

// Client holds the control plane clients of one origin.
type Client struct {
	APIURL string
	HTTP   *http.Client
	Agent  ownsandv1connect.AgentServiceClient
	Relay  ownsandv1connect.RelayAgentServiceClient

	// AuthTimeout bounds one Authenticate or ConfirmRemoval attempt, so a
	// control plane that accepts the call and then stalls ends the attempt
	// and the caller's back-off and retry run. The unary Enroll call has the
	// same deadline (EnrollContext). Zero means DefaultAuthTimeout. Only
	// these unary calls are bound: the agent channel and the relay
	// connection stream without a deadline.
	AuthTimeout time.Duration
}

func (c *Client) authTimeout() time.Duration {
	if c.AuthTimeout > 0 {
		return c.AuthTimeout
	}
	return DefaultAuthTimeout
}

// authContext bounds one authentication attempt with the authentication
// deadline.
func (c *Client) authContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.authTimeout())
}

// EnrollTimeout is the deadline of the unary Enroll call: AuthTimeout, or
// DefaultAuthTimeout when that is zero.
func (c *Client) EnrollTimeout() time.Duration { return c.authTimeout() }

// EnrollContext bounds the unary Enroll call with EnrollTimeout, so a
// control plane that accepts the request and then stalls does not hang the
// enroll command. An earlier deadline or cancellation of ctx still ends the
// call first.
func (c *Client) EnrollContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.EnrollTimeout())
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
	originHost, originPort := redirectHostname(u), effectivePort(u)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Keep the default net/http redirect limit.
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			target := req.URL
			if target.Scheme != u.Scheme || redirectHostname(target) != originHost || effectivePort(target) != originPort {
				return errors.New("cpclient: redirect leaves the control plane origin")
			}
			return nil
		},
	}, nil
}

func redirectHostname(u *url.URL) string {
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.String()
	}
	// Use the same IDNA lookup profile as net/http for DNS names.
	if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		return ascii
	}
	return host
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return strings.TrimLeft(port, "0")
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
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

// attemptError wraps the error of the step of one authentication attempt.
// When the attempt deadline ended it, and not the caller's context, the
// error says so and still matches context.DeadlineExceeded; it is never
// ErrUnknownDevice. The server sees the same deadline through the Connect
// timeout header and can answer deadline_exceeded just before the client
// timer fires, so that code counts as the deadline too.
func (c *Client) attemptError(parent, attempt context.Context, step string, err error) error {
	timedOut := errors.Is(attempt.Err(), context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded
	if parent.Err() == nil && timedOut {
		return fmt.Errorf("cpclient: %s: no answer within %s: %w", step, c.authTimeout(), context.DeadlineExceeded)
	}
	return fmt.Errorf("cpclient: %s: %w", step, err)
}

// ErrUnknownDevice reports a not_found answer of Authenticate or
// ConfirmRemoval: the control plane does not know the device.
var ErrUnknownDevice = errors.New("cpclient: the control plane does not know this device")

// AuthResult is a session or a removal notice.
type AuthResult struct {
	Session *Session
	Removal *ownsandv1.RemovalNotice
}

// challenge creates a nonce under attempt, which is the bounded context of
// the attempt that parent started.
func (c *Client) challenge(parent, attempt context.Context, deviceID string) ([]byte, error) {
	resp, err := c.Agent.CreateChallenge(attempt, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: deviceID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrUnknownDevice
		}
		return nil, c.attemptError(parent, attempt, "create challenge", err)
	}
	return resp.Msg.GetNonce(), nil
}

// Authenticate gets a new device session credential, or the removal notice.
// The challenge and the signed call share one attempt deadline
// (Client.AuthTimeout); an attempt that exceeds it returns an error that
// matches context.DeadlineExceeded, so the caller can back off and retry.
func (c *Client) Authenticate(ctx context.Context, deviceID string, key *identity.Key, stateEpoch string) (*AuthResult, error) {
	actx, cancel := c.authContext(ctx)
	defer cancel()
	nonce, err := c.challenge(ctx, actx, deviceID)
	if err != nil {
		return nil, err
	}
	resp, err := c.Agent.Authenticate(actx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
		DeviceId:   deviceID,
		Nonce:      nonce,
		Signature:  key.Sign(identity.PurposeAuthenticate, nonce),
		StateEpoch: stateEpoch,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrUnknownDevice
		}
		return nil, c.attemptError(ctx, actx, "authenticate", err)
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
// earlier confirmation whose response was lost. Like Authenticate, one
// attempt has the deadline Client.AuthTimeout.
func (c *Client) ConfirmRemoval(ctx context.Context, deviceID string, key *identity.Key) error {
	actx, cancel := c.authContext(ctx)
	defer cancel()
	nonce, err := c.challenge(ctx, actx, deviceID)
	if err != nil {
		return err
	}
	_, err = c.Agent.ConfirmRemoval(actx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{
		DeviceId:  deviceID,
		Nonce:     nonce,
		Signature: key.Sign(identity.PurposeConfirmRemoval, nonce),
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return ErrUnknownDevice
		}
		return c.attemptError(ctx, actx, "confirm removal", err)
	}
	return nil
}
