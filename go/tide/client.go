// SPDX-License-Identifier: Apache-2.0

// Package tide is the Go SDK of the tide public API. It gives programs, the
// tide CLI and the tide MCP server one client for the Connect RPC services
// of delino.tide.v1, with helpers for pagination, the queue watch, dev
// session commands and dev session file sync and download.
//
// The owning contract is docs/packages-tide-sdk-foundation.md. The SDK
// keeps credentials in memory only, sends them only to the configured API
// origin and logs nothing unless the caller passes a logger. It never logs
// request messages, credentials, command arguments, environment values,
// command output or file contents.
package tide

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
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
)

// DefaultBaseURL is the production API origin.
const DefaultBaseURL = "https://tide.api.delino.io"

// APIKeyPrefix is the fixed prefix of personal API keys. The server tells a
// personal API key and a Logto access token apart by it.
const APIKeyPrefix = "tide_"

// TokenSource returns the current credential: a Logto access token for the
// API resource https://tide.api.delino.io, or a personal API key. The SDK
// calls it before every RPC, so it should cache a token until shortly
// before it expires.
type TokenSource func(ctx context.Context) (string, error)

// Options configures a Client.
type Options struct {
	// BaseURL is the API origin, for example https://tide.api.delino.io or
	// http://127.0.0.1:18400 for a local control plane. The origin must
	// have a hostname. An http:// origin uses HTTP/2 without TLS (h2c
	// with prior knowledge) or HTTP/1.1. It must not have a path, query,
	// fragment or user info. Empty means DefaultBaseURL.
	BaseURL string
	// APIKey is a personal API key (tide_...). Set exactly one of APIKey and
	// TokenSource.
	APIKey string
	// TokenSource returns a Logto access token (or a personal API key).
	TokenSource TokenSource
	// HTTPClient overrides the HTTP client. The SDK keeps its transport and
	// timeout, never follows redirects and uses no cookie jar.
	HTTPClient *http.Client
	// Logger receives debug records about RPC failures, retries and
	// reconnects. Nil logs nothing.
	Logger *slog.Logger
}

// Client is a tide API client. It is safe for concurrent use.
type Client struct {
	baseURL    string
	credential credential
	logger     *slog.Logger
	timing     timing

	organizations     tidev1connect.OrganizationServiceClient
	members           tidev1connect.MemberServiceClient
	ownSandLinks      tidev1connect.OwnSandLinkServiceClient
	gitHub            tidev1connect.GitHubServiceClient
	agentIntegrations tidev1connect.AgentIntegrationServiceClient
	runnerProfiles    tidev1connect.RunnerProfileServiceClient
	work              tidev1connect.WorkServiceClient
	apiKeys           tidev1connect.ApiKeyServiceClient
	audit             tidev1connect.AuditServiceClient
	devSessions       tidev1connect.DevSessionServiceClient
}

// timing holds the delays of the helpers. Tests shorten them.
type timing struct {
	// backoffMin and backoffMax bound the reconnect delay of WatchQueue
	// and the retry delay of exec continuations.
	backoffMin, backoffMax time.Duration
	// retryBudget bounds how long an exec continuation or a file chunk
	// retries transient failures without progress.
	retryBudget time.Duration
	// waitInterval is the default polling interval of WaitDevSession.
	waitInterval time.Duration
}

func defaultTiming() timing {
	return timing{
		backoffMin:   time.Second,
		backoffMax:   30 * time.Second,
		retryBudget:  2 * time.Minute,
		waitInterval: 2 * time.Second,
	}
}

// ErrNoCredential reports a client without an API key or token source.
var ErrNoCredential = errors.New("tide: no credential: set Options.APIKey or Options.TokenSource")

// New returns a client.
func New(opts Options) (*Client, error) {
	base := opts.BaseURL
	if base == "" {
		base = DefaultBaseURL
	}
	base = strings.TrimRight(base, "/")
	origin, err := url.Parse(base)
	// Host can contain a port even when Hostname is empty.
	if err != nil || origin.Hostname() == "" || (origin.Scheme != "https" && origin.Scheme != "http") {
		return nil, errors.New("tide: invalid base URL: want an http or https origin")
	}
	// url.Parse drops an empty fragment and records a bare "?" only as
	// ForceQuery, so check the raw string. The Connect clients append the
	// RPC path to the origin, which a query or fragment marker would
	// swallow.
	if strings.ContainsAny(base, "?#") || origin.Path != "" || origin.RawQuery != "" || origin.User != nil || origin.Opaque != "" {
		return nil, errors.New("tide: invalid base URL: want an origin without path, query, fragment or user info")
	}
	var cred credential
	switch {
	case opts.APIKey != "" && opts.TokenSource != nil:
		return nil, errors.New("tide: set only one of Options.APIKey and Options.TokenSource")
	case opts.APIKey != "":
		cred = staticCredential(opts.APIKey)
	case opts.TokenSource != nil:
		cred = tokenCredential(opts.TokenSource)
	default:
		return nil, ErrNoCredential
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	c := &Client{
		baseURL:    base,
		credential: cred,
		logger:     logger,
		timing:     defaultTiming(),
	}
	hc := newHTTPClient(opts.HTTPClient, origin)
	o := connect.WithInterceptors(&authInterceptor{client: c})
	c.organizations = tidev1connect.NewOrganizationServiceClient(hc, base, o)
	c.members = tidev1connect.NewMemberServiceClient(hc, base, o)
	c.ownSandLinks = tidev1connect.NewOwnSandLinkServiceClient(hc, base, o)
	c.gitHub = tidev1connect.NewGitHubServiceClient(hc, base, o)
	c.agentIntegrations = tidev1connect.NewAgentIntegrationServiceClient(hc, base, o)
	c.runnerProfiles = tidev1connect.NewRunnerProfileServiceClient(hc, base, o)
	c.work = tidev1connect.NewWorkServiceClient(hc, base, o)
	c.apiKeys = tidev1connect.NewApiKeyServiceClient(hc, base, o)
	c.audit = tidev1connect.NewAuditServiceClient(hc, base, o)
	c.devSessions = tidev1connect.NewDevSessionServiceClient(hc, base, o)
	return c, nil
}

// BaseURL returns the configured API origin.
func (c *Client) BaseURL() string { return c.baseURL }

// UsesAPIKey reports whether the client authenticates with a static
// personal API key.
func (c *Client) UsesAPIKey() bool {
	_, ok := c.credential.(staticCredential)
	return ok
}

// The service accessors return the generated Connect clients of
// delino.tide.v1. Every call sends the client's credential; errors are
// *connect.Error values with the server's Connect code.

// Organizations returns the OrganizationService client.
func (c *Client) Organizations() tidev1connect.OrganizationServiceClient { return c.organizations }

// Members returns the MemberService client.
func (c *Client) Members() tidev1connect.MemberServiceClient { return c.members }

// OwnSandLinks returns the OwnSandLinkService client.
func (c *Client) OwnSandLinks() tidev1connect.OwnSandLinkServiceClient { return c.ownSandLinks }

// GitHub returns the GitHubService client.
func (c *Client) GitHub() tidev1connect.GitHubServiceClient { return c.gitHub }

// AgentIntegrations returns the AgentIntegrationService client.
func (c *Client) AgentIntegrations() tidev1connect.AgentIntegrationServiceClient {
	return c.agentIntegrations
}

// RunnerProfiles returns the RunnerProfileService client.
func (c *Client) RunnerProfiles() tidev1connect.RunnerProfileServiceClient { return c.runnerProfiles }

// Work returns the WorkService client (requests, runners, capacity and the
// raw queue watch stream; prefer WatchQueue for the queue watch).
func (c *Client) Work() tidev1connect.WorkServiceClient { return c.work }

// APIKeys returns the ApiKeyService client.
func (c *Client) APIKeys() tidev1connect.ApiKeyServiceClient { return c.apiKeys }

// Audit returns the AuditService client.
func (c *Client) Audit() tidev1connect.AuditServiceClient { return c.audit }

// DevSessions returns the DevSessionService client. Prefer Exec, AttachExec,
// WaitDevSession, Sync and Download for commands, readiness and transfers.
func (c *Client) DevSessions() tidev1connect.DevSessionServiceClient { return c.devSessions }

// newHTTPClient returns the HTTP client of the SDK. An http:// origin may
// use HTTP/2 with prior knowledge (h2c) for server streams; an https://
// origin negotiates HTTP/2 with ALPN. The client never follows redirects,
// so the credential never leaves the configured origin.
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
		Transport: rt,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	if base != nil {
		hc.Timeout = base.Timeout
	}
	return hc
}

// errorf returns an SDK error with the "tide: " prefix.
func errorf(format string, args ...any) error {
	return fmt.Errorf("tide: "+format, args...)
}
