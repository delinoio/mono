// SPDX-License-Identifier: Apache-2.0

// Package ownsand is the Go SDK of the OwnSand public API. It gives programs
// and the ownsand CLI one client for the Connect RPC services of
// delino.ownsand.v1, a sandbox handle and the data path (a peer-to-peer
// WebRTC data channel with the relay of the control plane as the fallback).
//
// The owning contract is docs/packages-ownsand-sdk-foundation.md. The SDK
// keeps credentials in memory only, sends them only to the configured API
// origin and logs nothing unless the caller passes a logger. It never logs
// credentials, exec commands, exec output or file contents.
package ownsand

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// DefaultBaseURL is the production API origin. It also serves the relay.
const DefaultBaseURL = "https://ownsand.api.delino.io"

// Environment variables that the SDK reads when Options leaves the value
// empty. The SDK reads no other environment variables for credentials.
const (
	EnvAPIKey       = "OWNSAND_API_KEY"
	EnvOrganization = "OWNSAND_ORG"
)

// APIKeyPrefix is the fixed prefix of organization API keys.
const APIKeyPrefix = "osk_"

// RelayGrantHeader carries the relay grant ID on every RelayService request.
const RelayGrantHeader = "Ownsand-Relay-Grant-Id"

// TokenSource returns a current Logto access token. The SDK calls it before
// every RPC, so it should cache the token until shortly before it expires.
type TokenSource func(ctx context.Context) (string, error)

// Transport selects the data path transport of a client.
type Transport int

const (
	// TransportAuto tries the peer-to-peer data channel first and falls back
	// to the relay. It is the default.
	TransportAuto Transport = iota
	// TransportPeerToPeer forces the peer-to-peer data channel and never
	// falls back to the relay. Intended for tests.
	TransportPeerToPeer
	// TransportRelay forces the relay and never calls OpenSession.
	TransportRelay
)

// String returns the stable identifier of t.
func (t Transport) String() string {
	switch t {
	case TransportAuto:
		return "auto"
	case TransportPeerToPeer:
		return "peer-to-peer"
	case TransportRelay:
		return "relay"
	}
	return fmt.Sprintf("Transport(%d)", int(t))
}

// ParseTransport parses the identifiers that String returns.
func ParseTransport(s string) (Transport, error) {
	switch s {
	case "auto", "":
		return TransportAuto, nil
	case "peer-to-peer", "p2p":
		return TransportPeerToPeer, nil
	case "relay":
		return TransportRelay, nil
	}
	return TransportAuto, fmt.Errorf("ownsand: unknown transport %q (want auto, peer-to-peer or relay)", s)
}

// Options configures a Client.
type Options struct {
	// BaseURL is the API origin, for example https://ownsand.api.delino.io
	// or http://127.0.0.1:18300 for a local control plane. An http:// origin
	// uses HTTP/2 without TLS (h2c with prior knowledge). An explicit port
	// must be at most 65535. The origin must not have a path, query marker
	// ("?"), fragment marker ("#") or user info. Empty means DefaultBaseURL.
	BaseURL string
	// APIKey is an organization API key (osk_...). Set at most one of
	// APIKey and TokenSource. When both are empty, the client reads
	// OWNSAND_API_KEY.
	APIKey string
	// TokenSource returns a Logto access token.
	TokenSource TokenSource
	// OrganizationID is the default organization of every RPC that needs
	// one. Empty means OWNSAND_ORG. A request that sets organization_id
	// overrides it.
	OrganizationID string
	// HTTPClient overrides the HTTP client. The SDK wraps its transport to
	// observe received bytes and never follows redirects.
	HTTPClient *http.Client
	// Logger receives RPC names, IDs, transport selection, durations and
	// Connect codes. Nil logs nothing.
	Logger *slog.Logger
	// Transport selects the data path transport.
	Transport Transport
	// LookupEnv overrides os.LookupEnv, for tests.
	LookupEnv func(string) (string, bool)
}

// Client is an OwnSand API client. It is safe for concurrent use.
type Client struct {
	baseURL    string
	origin     *url.URL
	credential credential
	orgID      string
	logger     *slog.Logger
	transport  Transport
	timing     timing
	p2p        p2pConfig

	httpClient *http.Client

	organizations ownsandv1connect.OrganizationServiceClient
	members       ownsandv1connect.MemberServiceClient
	devices       ownsandv1connect.DeviceServiceClient
	apiKeys       ownsandv1connect.ApiKeyServiceClient
	sandboxes     ownsandv1connect.SandboxServiceClient
	snapshots     ownsandv1connect.SnapshotServiceClient
	dataPath      ownsandv1connect.DataPathServiceClient
	relay         ownsandv1connect.RelayServiceClient
	billing       ownsandv1connect.BillingServiceClient
}

// ErrNoCredential reports a client without an API key or token source.
var ErrNoCredential = errors.New("ownsand: no credential: set Options.APIKey, Options.TokenSource or OWNSAND_API_KEY")

// ErrNoOrganization reports an RPC that needs an organization when neither
// the request, the client nor OWNSAND_ORG names one. The SDK returns it
// before it sends the request.
var ErrNoOrganization = errors.New("ownsand: no organization: set the organization ID, Options.OrganizationID or OWNSAND_ORG")

var errInvalidBaseURL = errors.New("ownsand: invalid base URL: want an http or https origin")

// New returns a client.
func New(opts Options) (*Client, error) {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	origin, err := url.Parse(base)
	// Hostname drops the port, so "http://:18300" has a nonempty Host but no
	// hostname.
	if err != nil || origin.Hostname() == "" || (origin.Scheme != "https" && origin.Scheme != "http") {
		return nil, errInvalidBaseURL
	}
	// url.Parse drops an empty fragment and records a bare "?" only as
	// ForceQuery, so check the raw string. The Connect clients append the RPC
	// path to it, which a query marker or fragment would swallow.
	if strings.ContainsAny(base, "?#") || origin.Path != "" || origin.RawQuery != "" || origin.User != nil || origin.Opaque != "" {
		return nil, fmt.Errorf("ownsand: invalid base URL: want an origin without path, query, fragment or user info")
	}
	if port := origin.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return nil, errInvalidBaseURL
		}
	}

	var cred credential
	switch {
	case opts.APIKey != "" && opts.TokenSource != nil:
		return nil, errors.New("ownsand: set only one of Options.APIKey and Options.TokenSource")
	case opts.APIKey != "":
		cred = staticCredential(opts.APIKey)
	case opts.TokenSource != nil:
		cred = tokenCredential(opts.TokenSource)
	default:
		if v, ok := lookup(EnvAPIKey); ok && v != "" {
			cred = staticCredential(v)
		} else {
			return nil, ErrNoCredential
		}
	}
	org := opts.OrganizationID
	if org == "" {
		if v, ok := lookup(EnvOrganization); ok {
			org = v
		}
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	c := &Client{
		baseURL:    base,
		origin:     origin,
		credential: cred,
		orgID:      org,
		logger:     logger,
		transport:  opts.Transport,
		timing:     defaultTiming(),
	}
	c.httpClient = newHTTPClient(opts.HTTPClient, origin)

	interceptors := connect.WithInterceptors(&authInterceptor{client: c})
	c.organizations = ownsandv1connect.NewOrganizationServiceClient(c.httpClient, base, interceptors)
	c.members = ownsandv1connect.NewMemberServiceClient(c.httpClient, base, interceptors)
	c.devices = ownsandv1connect.NewDeviceServiceClient(c.httpClient, base, interceptors)
	c.apiKeys = ownsandv1connect.NewApiKeyServiceClient(c.httpClient, base, interceptors)
	c.sandboxes = ownsandv1connect.NewSandboxServiceClient(c.httpClient, base, interceptors)
	c.snapshots = ownsandv1connect.NewSnapshotServiceClient(c.httpClient, base, interceptors)
	c.dataPath = ownsandv1connect.NewDataPathServiceClient(c.httpClient, base, interceptors)
	c.relay = ownsandv1connect.NewRelayServiceClient(c.httpClient, base, interceptors)
	c.billing = ownsandv1connect.NewBillingServiceClient(c.httpClient, base, interceptors)
	return c, nil
}

// BaseURL returns the configured API origin.
func (c *Client) BaseURL() string { return c.baseURL }

// OrganizationID returns the default organization, or "".
func (c *Client) OrganizationID() string { return c.orgID }

// UsesAPIKey reports whether the client authenticates with an API key.
func (c *Client) UsesAPIKey() bool {
	_, ok := c.credential.(staticCredential)
	return ok
}

// organization returns override, or the client default, or ErrNoOrganization.
func (c *Client) organization(override string) (string, error) {
	if override != "" {
		return override, nil
	}
	if c.orgID != "" {
		return c.orgID, nil
	}
	return "", ErrNoOrganization
}

// newHTTPClient returns the HTTP client of the SDK. An http:// origin uses
// HTTP/2 with prior knowledge (h2c), because the control plane serves h2c
// and the relay needs HTTP/2; an https:// origin negotiates HTTP/2 with
// ALPN. The client never follows redirects, so the credential never leaves
// the configured origin, and the transport records received bytes for the
// relay stall detection.
func newHTTPClient(base *http.Client, origin *url.URL) *http.Client {
	var rt http.RoundTripper
	if base != nil && base.Transport != nil {
		rt = base.Transport
	} else {
		t := &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     true,
			Protocols:             new(http.Protocols),
		}
		if origin.Scheme == "http" {
			t.Protocols.SetUnencryptedHTTP2(true)
		} else {
			t.Protocols.SetHTTP1(true)
			t.Protocols.SetHTTP2(true)
		}
		rt = t
	}
	hc := &http.Client{
		Transport: &activityTransport{next: rt},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if base != nil {
		hc.Jar = nil
		hc.Timeout = base.Timeout
	}
	return hc
}
