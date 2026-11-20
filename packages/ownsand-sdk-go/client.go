// Package ownsand is the Go SDK of OwnSand. It wraps the public Connect RPC
// services of the control plane, gives each sandbox a handle, and runs exec
// and file operations over a peer-to-peer WebRTC data channel to the device
// agent with the control plane relay as the fallback.
//
// The SDK persists nothing and logs nothing by default. It never logs or
// returns its credential in errors, and it sends the credential only to the
// configured API origin.
package ownsand

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// DefaultOrigin is the production API origin. It also serves the relay.
const DefaultOrigin = "https://ownsand.api.delino.io"

// Environment variables that the SDK reads.
const (
	// EnvAPIKey holds an organization API key. It is the only environment
	// variable that the SDK reads for credentials.
	EnvAPIKey = "OWNSAND_API_KEY"
	// EnvOrganization holds the default organization ID.
	EnvOrganization = "OWNSAND_ORG"
)

// TokenFunc returns a current Logto access token. The SDK calls it for every
// request, so it can refresh the token.
type TokenFunc func(ctx context.Context) (string, error)

// TransportMode selects the data path of exec and file operations.
type TransportMode int

const (
	// TransportAuto tries the peer-to-peer data channel first and falls back
	// to the relay.
	TransportAuto TransportMode = iota
	// TransportPeerToPeer forces the peer-to-peer data channel (for tests).
	// Operations fail when the channel cannot open.
	TransportPeerToPeer
	// TransportRelay forces the relay. The SDK then never calls OpenSession.
	TransportRelay
)

// Options configure a Client.
type Options struct {
	// Origin is the API origin, for example https://ownsand.api.delino.io.
	// Empty means DefaultOrigin.
	Origin string
	// APIKey is an organization API key (osk_...).
	APIKey string
	// Token returns a Logto access token. Exactly one of APIKey and Token may
	// be set; without both, the SDK reads OWNSAND_API_KEY.
	Token TokenFunc
	// OrganizationID is the default organization. Empty means OWNSAND_ORG.
	OrganizationID string
	// HTTPClient is the base client. The SDK wraps its transport with the
	// origin guard and does not follow redirects. Nil means a new client.
	HTTPClient *http.Client
	// Logger receives RPC names, IDs, transport selection, durations and
	// Connect codes, never credentials or sandbox content. Nil logs nothing.
	Logger *slog.Logger
	// Clock is the time source of data path timers. Nil means clock.Real.
	Clock clock.Clock
	// Transport selects the data path. The default is TransportAuto.
	Transport TransportMode

	// peerSetup lets in-package tests adjust the WebRTC setting engine (for
	// example, to allow loopback candidates).
	peerSetup peerSetupFunc
}

// ErrNoCredential is returned by New when no credential is configured.
var ErrNoCredential = errors.New("ownsand: no credential: set an API key, a token function or OWNSAND_API_KEY")

// ErrNoOrganization is returned before an RPC that needs an organization
// when neither the call, the client nor OWNSAND_ORG names one.
var ErrNoOrganization = errors.New("ownsand: no organization: set the organization ID or OWNSAND_ORG")

// Client is an OwnSand API client. It is safe for concurrent use.
type Client struct {
	origin    *url.URL
	org       string
	apiKey    string
	token     TokenFunc
	log       *slog.Logger
	clk       clock.Clock
	transport TransportMode
	peerSetup peerSetupFunc
	http      *http.Client

	secretsMu sync.Mutex
	secrets   []string // credentials seen, for redaction

	orgs      ownsandv1connect.OrganizationServiceClient
	members   ownsandv1connect.MemberServiceClient
	apiKeys   ownsandv1connect.ApiKeyServiceClient
	devices   ownsandv1connect.DeviceServiceClient
	sandboxes ownsandv1connect.SandboxServiceClient
	snapshots ownsandv1connect.SnapshotServiceClient
	dataPath  ownsandv1connect.DataPathServiceClient
	relay     ownsandv1connect.RelayServiceClient
}

// New returns a Client.
func New(opts Options) (*Client, error) {
	origin := opts.Origin
	if origin == "" {
		origin = DefaultOrigin
	}
	u, err := parseOrigin(origin)
	if err != nil {
		return nil, err
	}
	if opts.APIKey != "" && opts.Token != nil {
		return nil, errors.New("ownsand: set either an API key or a token function, not both")
	}
	apiKey := opts.APIKey
	if apiKey == "" && opts.Token == nil {
		apiKey = os.Getenv(EnvAPIKey)
	}
	if apiKey == "" && opts.Token == nil {
		return nil, ErrNoCredential
	}
	org := opts.OrganizationID
	if org == "" {
		org = os.Getenv(EnvOrganization)
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real()
	}
	c := &Client{
		origin:    u,
		org:       org,
		apiKey:    apiKey,
		token:     opts.Token,
		log:       logger,
		clk:       clk,
		transport: opts.Transport,
		peerSetup: opts.peerSetup,
	}
	if apiKey != "" {
		c.secrets = []string{apiKey}
	}
	base := http.DefaultTransport
	if opts.HTTPClient != nil && opts.HTTPClient.Transport != nil {
		base = opts.HTTPClient.Transport
	} else if u.Scheme == "http" {
		// Cleartext origins (local development and tests) use HTTP/2
		// without TLS (h2c), like the HTTP/2 route in production.
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.Protocols = new(http.Protocols)
		t.Protocols.SetUnencryptedHTTP2(true)
		base = t
	}
	hc := &http.Client{
		Transport: &originGuard{c: c, base: base},
		// Never follow redirects: a redirect could send the credential to
		// another origin.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	if opts.HTTPClient != nil {
		hc.Jar = opts.HTTPClient.Jar
		hc.Timeout = opts.HTTPClient.Timeout
	}
	c.http = hc
	o := u.String()
	copts := []connect.ClientOption{connect.WithInterceptors(&logInterceptor{c: c})}
	c.orgs = ownsandv1connect.NewOrganizationServiceClient(hc, o, copts...)
	c.members = ownsandv1connect.NewMemberServiceClient(hc, o, copts...)
	c.apiKeys = ownsandv1connect.NewApiKeyServiceClient(hc, o, copts...)
	c.devices = ownsandv1connect.NewDeviceServiceClient(hc, o, copts...)
	c.sandboxes = ownsandv1connect.NewSandboxServiceClient(hc, o, copts...)
	c.snapshots = ownsandv1connect.NewSnapshotServiceClient(hc, o, copts...)
	c.dataPath = ownsandv1connect.NewDataPathServiceClient(hc, o, copts...)
	c.relay = ownsandv1connect.NewRelayServiceClient(hc, o, copts...)
	return c, nil
}

func parseOrigin(s string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSuffix(s, "/"))
	if err != nil {
		return nil, fmt.Errorf("ownsand: invalid origin: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.User != nil {
		return nil, fmt.Errorf("ownsand: origin must be scheme://host[:port], got %q", s)
	}
	return &url.URL{Scheme: u.Scheme, Host: u.Host}, nil
}

// Origin returns the configured API origin.
func (c *Client) Origin() string { return c.origin.String() }

// OrganizationID returns the default organization ID, or an empty string.
func (c *Client) OrganizationID() string { return c.org }

// CallOption changes one call.
type CallOption func(*callOptions)

type callOptions struct {
	org string
}

// WithOrganization overrides the organization of one call.
func WithOrganization(id string) CallOption { return func(o *callOptions) { o.org = id } }

func (c *Client) orgFor(opts []CallOption) (string, error) {
	var co callOptions
	for _, o := range opts {
		o(&co)
	}
	if co.org != "" {
		return co.org, nil
	}
	if c.org != "" {
		return c.org, nil
	}
	return "", ErrNoOrganization
}

func (c *Client) credential(ctx context.Context) (string, error) {
	if c.apiKey != "" {
		return c.apiKey, nil
	}
	tok, err := c.token(ctx)
	if err != nil {
		return "", fmt.Errorf("ownsand: token function: %w", err)
	}
	if tok == "" {
		return "", errors.New("ownsand: token function returned an empty token")
	}
	c.secretsMu.Lock()
	known := false
	for _, s := range c.secrets {
		if s == tok {
			known = true
			break
		}
	}
	if !known {
		// Keep a bounded list of recent tokens for redaction.
		c.secrets = append(c.secrets, tok)
		if len(c.secrets) > 8 {
			c.secrets = c.secrets[len(c.secrets)-8:]
		}
	}
	c.secretsMu.Unlock()
	return tok, nil
}

// redact returns err with every known credential removed from its message.
// It keeps the Connect code.
func (c *Client) redact(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	c.secretsMu.Lock()
	secrets := append([]string(nil), c.secrets...)
	c.secretsMu.Unlock()
	changed := false
	for _, s := range secrets {
		if s != "" && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "[REDACTED]")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return &redactedError{msg: msg, code: connect.CodeOf(err)}
}

type redactedError struct {
	msg  string
	code connect.Code
}

func (e *redactedError) Error() string { return e.msg }

// Code returns the Connect code of the original error.
func (e *redactedError) Code() connect.Code { return e.code }

// CodeOf returns the Connect code of an SDK error, also of a redacted one.
func CodeOf(err error) connect.Code {
	var re *redactedError
	if errors.As(err, &re) {
		return re.code
	}
	var fe *remoteError
	if errors.As(err, &fe) {
		return fe.code
	}
	return connect.CodeOf(err)
}

// originGuard adds the credential to requests for the configured origin and
// refuses every other request, so the credential never leaves the origin.
type originGuard struct {
	c    *Client
	base http.RoundTripper
}

var errForeignOrigin = errors.New("ownsand: refused a request outside the configured API origin")

func (g *originGuard) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != g.c.origin.Scheme || req.URL.Host != g.c.origin.Host {
		return nil, errForeignOrigin
	}
	cred, err := g.c.credential(req.Context())
	if err != nil {
		return nil, err
	}
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+cred)
	resp, err := g.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if a := activityFrom(req.Context()); a != nil && resp.Body != nil {
		resp.Body = &activityBody{ReadCloser: resp.Body, a: a}
	}
	return resp, nil
}

// logInterceptor logs RPC names, durations and codes. It never logs
// messages, headers or credentials.
type logInterceptor struct{ c *Client }

func (l *logInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		start := l.c.clk.Now()
		resp, err := next(ctx, req)
		l.c.log.LogAttrs(ctx, slog.LevelDebug, "ownsand rpc",
			slog.String("rpc", req.Spec().Procedure),
			slog.Duration("duration", l.c.clk.Now().Sub(start)),
			slog.String("code", codeName(err)))
		return resp, err
	}
}

func (l *logInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		l.c.log.LogAttrs(ctx, slog.LevelDebug, "ownsand stream open", slog.String("rpc", spec.Procedure))
		return next(ctx, spec)
	}
}

func (l *logInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func codeName(err error) string {
	if err == nil {
		return "ok"
	}
	return connect.CodeOf(err).String()
}
