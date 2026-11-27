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

type authAttempt struct {
	ctx    context.Context
	parent context.Context
	// timeout is zero when the caller supplies the effective deadline.
	timeout time.Duration
}

func (c *Client) startAuthAttempt(parent context.Context) (authAttempt, context.CancelFunc) {
	timeout := c.authTimeout()
	deadline := time.Now().Add(timeout)
	if callerDeadline, ok := parent.Deadline(); ok && !deadline.Before(callerDeadline) {
		ctx, cancel := context.WithCancel(parent)
		return authAttempt{ctx: ctx, parent: parent}, cancel
	}
	ctx, cancel := context.WithDeadline(parent, deadline)
	return authAttempt{ctx: ctx, parent: parent, timeout: timeout}, cancel
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
		// The parse error includes the input URL, which may contain credentials.
		return nil, errors.New("cpclient: invalid control plane origin URL")
	}
	var protocols http.Protocols
	switch u.Scheme {
	case "https":
		protocols.SetHTTP2(true)
	case "http":
		protocols.SetUnencryptedHTTP2(true)
	default:
		return nil, errors.New("cpclient: control plane origin must use https or http")
	}
	if u.Hostname() == "" {
		return nil, errors.New("cpclient: control plane origin requires a hostname")
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

// stepError identifies the failing RPC. Connect sends the effective deadline
// to the server, which can return deadline_exceeded before the local timer fires.
func (a authAttempt) stepError(step string, err error) error {
	timedOut := errors.Is(a.ctx.Err(), context.DeadlineExceeded) || connect.CodeOf(err) == connect.CodeDeadlineExceeded
	if a.timeout > 0 && a.parent.Err() == nil && timedOut {
		return fmt.Errorf("cpclient: %s: no answer within %s: %w", step, a.timeout, context.DeadlineExceeded)
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

// challenge creates a nonce under the shared attempt deadline.
func (c *Client) challenge(attempt authAttempt, deviceID string) ([]byte, error) {
	resp, err := c.Agent.CreateChallenge(attempt.ctx, connect.NewRequest(&ownsandv1.CreateChallengeRequest{DeviceId: deviceID}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrUnknownDevice
		}
		return nil, attempt.stepError("create challenge", err)
	}
	return resp.Msg.GetNonce(), nil
}

// Authenticate gets a new device session credential, or the removal notice.
// The challenge and the signed call share one deadline. When the agent's
// timeout controls the attempt, a timeout error matches context.DeadlineExceeded.
func (c *Client) Authenticate(ctx context.Context, deviceID string, key *identity.Key, stateEpoch string) (*AuthResult, error) {
	attempt, cancel := c.startAuthAttempt(ctx)
	defer cancel()
	nonce, err := c.challenge(attempt, deviceID)
	if err != nil {
		return nil, err
	}
	resp, err := c.Agent.Authenticate(attempt.ctx, connect.NewRequest(&ownsandv1.AuthenticateRequest{
		DeviceId:   deviceID,
		Nonce:      nonce,
		Signature:  key.Sign(identity.PurposeAuthenticate, nonce),
		StateEpoch: stateEpoch,
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return nil, ErrUnknownDevice
		}
		return nil, attempt.stepError("authenticate", err)
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
// earlier confirmation whose response was lost. Like Authenticate, both
// RPCs share one attempt deadline.
func (c *Client) ConfirmRemoval(ctx context.Context, deviceID string, key *identity.Key) error {
	attempt, cancel := c.startAuthAttempt(ctx)
	defer cancel()
	nonce, err := c.challenge(attempt, deviceID)
	if err != nil {
		return err
	}
	_, err = c.Agent.ConfirmRemoval(attempt.ctx, connect.NewRequest(&ownsandv1.ConfirmRemovalRequest{
		DeviceId:  deviceID,
		Nonce:     nonce,
		Signature: key.Sign(identity.PurposeConfirmRemoval, nonce),
	}))
	if err != nil {
		if connect.CodeOf(err) == connect.CodeNotFound {
			return ErrUnknownDevice
		}
		return attempt.stepError("confirm removal", err)
	}
	return nil
}
