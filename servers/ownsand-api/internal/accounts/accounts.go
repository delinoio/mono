// SPDX-License-Identifier: FSL-1.1-ALv2

// Package accounts is the control plane's client of the delino-accounts
// API: a Logto machine-to-machine access token source (the client
// credentials grant) and a UsageService client that sends the token. The
// owning contracts are the Billing section of
// docs/servers-ownsand-api-foundation.md and
// docs/protos-delino-accounts-api-contract.md.
package accounts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
)

// Logto API resource and scope of the delino-accounts usage API.
const (
	Resource         = "https://accounts.api.delino.io"
	ScopeReportUsage = "accounts:report-usage"
)

// Token caching.
const (
	// refreshMargin is how long before its expiry a cached token is
	// replaced. A token that lives less than twice the margin is replaced
	// after half of its lifetime.
	refreshMargin = time.Minute
	// maxTokenResponse bounds the token endpoint response that is read.
	maxTokenResponse = 64 << 10
	// tokenRequestTimeout bounds one token request, whatever the timeout of
	// the configured HTTP client.
	tokenRequestTimeout = 30 * time.Second
)

// TokenConfig configures a TokenSource.
type TokenConfig struct {
	// Endpoint is the Logto token endpoint, `<tenant endpoint>/oidc/token`.
	Endpoint     string
	ClientID     string
	ClientSecret string
	// Resource and Scope default to Resource and ScopeReportUsage.
	Resource string
	Scope    string
	// HTTPClient defaults to a client with a 30-second timeout.
	HTTPClient *http.Client
	// Now defaults to time.Now.
	Now func() time.Time
}

// TokenSource returns Logto machine-to-machine access tokens from the client
// credentials grant and caches each token until shortly before it expires.
// It is safe for concurrent use; concurrent callers share one token request
// and each caller waits for it only as long as its own context lives.
type TokenSource struct {
	cfg TokenConfig

	// mu guards only the fields below and is never held across a request.
	mu        sync.Mutex
	token     string
	refreshAt time.Time
	// flight is the token request in progress, or nil.
	flight *tokenFlight
}

// tokenFlight is one token request that all concurrent callers share. The
// result fields are written before done is closed and read only after it.
type tokenFlight struct {
	done  chan struct{}
	token string
	err   error
}

// NewTokenSource returns a token source for cfg.
func NewTokenSource(cfg TokenConfig) *TokenSource {
	if cfg.Resource == "" {
		cfg.Resource = Resource
	}
	if cfg.Scope == "" {
		cfg.Scope = ScopeReportUsage
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &TokenSource{cfg: cfg}
}

// TokenError is a failed token request. It never contains the client
// secret or a token.
type TokenError struct {
	Status int
	// Code is the OAuth error code of the response, when it has one.
	Code string
}

func (e *TokenError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("token endpoint returned HTTP %d (%s)", e.Status, e.Code)
	}
	return fmt.Sprintf("token endpoint returned HTTP %d", e.Status)
}

// Token returns a cached token or requests a new one. A caller that finds a
// request in progress waits for it and returns early with its own context
// error when ctx ends first.
//
// The request does not use the context of the caller that started it: it
// runs detached from every caller context (context.WithoutCancel) with a
// tokenRequestTimeout limit, so a caller that gives up never fails the
// callers that share the request. This is the same choice as the Logto key
// set refresh of the auth package. Remove it only together with a way for
// the last waiting caller to cancel the request.
func (s *TokenSource) Token(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	s.mu.Lock()
	if s.token != "" && s.cfg.Now().Before(s.refreshAt) {
		token := s.token
		s.mu.Unlock()
		return token, nil
	}
	f := s.flight
	if f == nil {
		f = &tokenFlight{done: make(chan struct{})}
		s.flight = f
		go s.refresh(context.WithoutCancel(ctx), f, s.cfg.Now())
	}
	s.mu.Unlock()
	select {
	case <-f.done:
		return f.token, f.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// refresh runs the token request of f and publishes its result. It caches the
// token unless Invalidate ran in the meantime.
func (s *TokenSource) refresh(ctx context.Context, f *tokenFlight, now time.Time) {
	ctx, cancel := context.WithTimeout(ctx, tokenRequestTimeout)
	defer cancel()
	token, lifetime, err := s.fetch(ctx)
	s.mu.Lock()
	if err == nil && s.flight == f {
		margin := refreshMargin
		if lifetime < 2*margin {
			margin = lifetime / 2
		}
		s.token, s.refreshAt = token, now.Add(lifetime-margin)
	}
	if s.flight == f {
		s.flight = nil
	}
	s.mu.Unlock()
	f.token, f.err = token, err
	close(f.done)
}

// Invalidate drops the cached token, for example after the API rejected it.
// It never waits for a token request: a request in progress still answers its
// callers, but its token is not cached, and the next Token call starts a new
// request.
func (s *TokenSource) Invalidate() {
	s.mu.Lock()
	s.token, s.refreshAt = "", time.Time{}
	s.flight = nil
	s.mu.Unlock()
}

func (s *TokenSource) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"resource":   {s.cfg.Resource},
		"scope":      {s.cfg.Scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic (RFC 6749 section 2.3.1): the ID and the secret
	// are form-encoded before the Basic encoding.
	req.SetBasicAuth(url.QueryEscape(s.cfg.ClientID), url.QueryEscape(s.cfg.ClientSecret))
	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTokenResponse))
	if err != nil {
		return "", 0, fmt.Errorf("read token response: %w", err)
	}
	var tr struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
	}
	_ = json.Unmarshal(body, &tr)
	if resp.StatusCode != http.StatusOK {
		return "", 0, &TokenError{Status: resp.StatusCode, Code: oauthCode(tr.Error)}
	}
	if tr.AccessToken == "" || (tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "Bearer")) {
		return "", 0, errors.New("token endpoint returned no bearer access token")
	}
	lifetime := time.Duration(tr.ExpiresIn) * time.Second
	if tr.ExpiresIn <= 0 || tr.ExpiresIn > int64(24*time.Hour/time.Second) {
		// Without a usable expiry, cache the token only briefly.
		lifetime = 2 * refreshMargin
	}
	return tr.AccessToken, lifetime, nil
}

// oauthCode keeps an OAuth error code only when it has the shape of one, so
// an unexpected response body never reaches the logs.
func oauthCode(code string) string {
	if len(code) == 0 || len(code) > 64 {
		return ""
	}
	for _, r := range code {
		if (r < 'a' || r > 'z') && r != '_' {
			return ""
		}
	}
	return code
}

// bearerInterceptor adds the machine-to-machine access token to each
// request and drops a cached token that the API rejected.
type bearerInterceptor struct{ tokens *TokenSource }

func (b bearerInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		token, err := b.tokens.Token(ctx)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnavailable, fmt.Errorf("get access token: %w", err))
		}
		req.Header().Set("Authorization", "Bearer "+token)
		resp, err := next(ctx, req)
		if connect.CodeOf(err) == connect.CodeUnauthenticated {
			b.tokens.Invalidate()
		}
		return resp, err
	}
}

func (b bearerInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (b bearerInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// NewUsageClient returns a UsageService client for the API origin that
// authenticates with tokens. A nil httpClient selects a client with a
// 30-second timeout.
func NewUsageClient(httpClient *http.Client, origin string, tokens *TokenSource) accountsv1connect.UsageServiceClient {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	return accountsv1connect.NewUsageServiceClient(httpClient, strings.TrimSuffix(origin, "/"),
		connect.WithInterceptors(bearerInterceptor{tokens: tokens}))
}
