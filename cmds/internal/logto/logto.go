// SPDX-License-Identifier: Apache-2.0

// Package logto implements the OAuth 2.0 device authorization flow
// (RFC 8628) and the refresh token grant against a Logto tenant, as the
// ownsand and SandRun CLIs use them: each CLI is a Logto native application with
// device flow, asks for offline_access in the device authorization and
// exchanges the refresh token for an access token of its API resource.
// See https://docs.logto.io/quick-starts/device-flow.
//
// Tokens and device codes are secrets: errors never contain them.
package logto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/idna"
)

// Config identifies the tenant, the application and the API resource.
type Config struct {
	// Endpoint is an HTTP or HTTPS tenant URL without a query or fragment.
	Endpoint string
	// AppID is the ID of the native application with device flow.
	AppID string
	// Resource is the API resource indicator of the API.
	Resource string
	// Scopes are requested in the device authorization; openid and
	// offline_access are always included.
	Scopes []string
	// HTTPClient overrides the HTTP client.
	HTTPClient *http.Client
}

// DeviceAuthorization is the response of the device authorization request.
type DeviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int    `json:"expires_in"`
	Interval                int    `json:"interval"`
	// ExpiresAt is the local expiry deadline of the device code. It is not
	// part of the response: StartDeviceAuthorization sets it to the time it
	// received the response plus ExpiresIn, so time that passes before
	// PollToken starts (for example while the caller writes the sign-in
	// prompt) counts against the code lifetime. The zero value makes
	// PollToken measure ExpiresIn from its own start.
	ExpiresAt time.Time `json:"-"`
}

// Token is a token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// ErrorCode is a recognized OAuth error code.
type ErrorCode string

const (
	// InvalidGrant can indicate a spent refresh token.
	InvalidGrant ErrorCode = "invalid_grant"

	authorizationPending ErrorCode = "authorization_pending"
	slowDown             ErrorCode = "slow_down"
	accessDenied         ErrorCode = "access_denied"
	expiredToken         ErrorCode = "expired_token"
)

// Error is an OAuth error response with the HTTP status and a recognized code,
// if present.
type Error struct {
	Status int
	code   ErrorCode
}

// Code returns the recognized OAuth code or an empty value.
func (e *Error) Code() ErrorCode { return e.code }

func (e *Error) Error() string {
	if e.code != "" {
		return "logto: " + string(e.code)
	}
	return fmt.Sprintf("logto: http_%d", e.Status)
}

func oauthError(status int, body []byte) *Error {
	e := &Error{Status: status}
	var response struct {
		Code ErrorCode `json:"error"`
	}
	if json.Unmarshal(body, &response) == nil {
		// Allow only the token and device-flow error codes from RFC 6749
		// section 5.2 and RFC 8628 section 3.5.
		switch response.Code {
		case "invalid_request", "invalid_client", InvalidGrant, "unauthorized_client",
			"unsupported_grant_type", "invalid_scope",
			authorizationPending, slowDown, accessDenied, expiredToken:
			e.code = response.Code
		}
	}
	return e
}

// ErrMissingConfig reports a configuration without a tenant endpoint or an
// application ID. Callers name their own configuration variables with it.
var ErrMissingConfig = errors.New("logto: the tenant endpoint and the application ID are required")

// ErrAccessDenied and ErrExpired end the device flow.
var (
	ErrAccessDenied = errors.New("logto: the sign-in was denied")
	ErrExpired      = errors.New("logto: the device code expired before the sign-in finished")
)

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultClient()
}

const (
	// maxRedirects keeps the redirect limit of the default HTTP client.
	maxRedirects = 10
	// defaultRequestTimeout bounds a whole request of the default client.
	defaultRequestTimeout = 30 * time.Second
	// defaultDialTimeout bounds establishing the connection of the default
	// client. It must stay below defaultRequestTimeout: the overall client
	// timeout would otherwise end a stuck connect first, with an error that
	// holds no *net.OpError, and PollToken could not tell the connection
	// timeout (which it backs off on) from a response timeout.
	defaultDialTimeout = 10 * time.Second
)

// defaultClient returns the shared client that is used when
// Config.HTTPClient is nil.
var defaultClient = sync.OnceValue(func() *http.Client {
	return newDefaultClient(defaultDialTimeout)
})

// newDefaultClient returns a client with the overall defaultRequestTimeout
// and a connect timeout of dialTimeout. It clones http.DefaultTransport, so
// the proxy, TLS handshake and HTTP/2 settings stay the same.
func newDefaultClient(dialTimeout time.Duration) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	dialer := &net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	tr.DialContext = dialer.DialContext
	return &http.Client{Timeout: defaultRequestTimeout, Transport: tr}
}

func (c Config) endpoint(path string) string {
	return strings.TrimRight(c.Endpoint, "/") + path
}

func (c Config) scope() string {
	seen := map[string]bool{}
	var out []string
	for _, s := range append([]string{"openid", "offline_access"}, c.Scopes...) {
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return strings.Join(out, " ")
}

var errRedirectOrigin = errors.New("logto: redirect leaves the configured tenant origin")

type origin struct {
	scheme string
	host   string
	port   string
}

func requestOrigin(u *url.URL) origin {
	host := u.Hostname()
	if ip, err := netip.ParseAddr(host); err == nil {
		host = ip.String()
	} else if ascii, err := idna.Lookup.ToASCII(host); err == nil {
		// Use the lookup profile that net/http uses for DNS names.
		host = strings.ToLower(ascii)
	}
	port := u.Port()
	if port != "" {
		port = strings.TrimLeft(port, "0")
	} else if u.Scheme == "https" {
		port = "443"
	} else if u.Scheme == "http" {
		port = "80"
	}
	return origin{scheme: u.Scheme, host: host, port: port}
}

func (c Config) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// Copy the client so this request can enforce the tenant origin while
	// preserving the caller's transport, timeouts, and redirect policy.
	client := *c.client()
	checkRedirect := client.CheckRedirect
	tenant := requestOrigin(req.URL)
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if requestOrigin(next.URL) != tenant {
			return errRedirectOrigin
		}
		if len(via) >= maxRedirects {
			return fmt.Errorf("stopped after %d redirects", maxRedirects)
		}
		if checkRedirect != nil {
			if err := checkRedirect(next, via); err != nil {
				return err
			}
			// A caller policy can edit the proposed request.
			if next.URL == nil || requestOrigin(next.URL) != tenant {
				return errRedirectOrigin
			}
		}
		return nil
	}
	res, err := client.Do(req)
	if errors.Is(err, errRedirectOrigin) {
		// net/http wraps redirect errors with the destination URL.
		return errRedirectOrigin
	}
	if err != nil {
		return fmt.Errorf("logto: %s: %w", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("logto: %s: %w", path, err)
	}
	if res.StatusCode/100 != 2 {
		return oauthError(res.StatusCode, body)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("logto: %s: invalid response", path)
	}
	return nil
}

// Validate checks that the configuration names a tenant and an application.
func (c Config) Validate() error {
	if c.Endpoint == "" || c.AppID == "" {
		return ErrMissingConfig
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return errors.New("logto: the tenant endpoint must be an http or https URL")
	}
	// Check for delimiters in the original URL because url.Parse drops an empty fragment.
	if strings.ContainsAny(c.Endpoint, "?#") {
		return errors.New("logto: the tenant endpoint must not contain a query or fragment")
	}
	if port := u.Port(); port != "" {
		if _, err := strconv.ParseUint(port, 10, 16); err != nil {
			return errors.New("logto: the tenant endpoint port must be at most 65535")
		}
	}
	return nil
}

// StartDeviceAuthorization requests a device code and a user code.
func (c Config) StartDeviceAuthorization(ctx context.Context) (*DeviceAuthorization, error) {
	form := url.Values{"client_id": {c.AppID}, "scope": {c.scope()}}
	if c.Resource != "" {
		form.Set("resource", c.Resource)
	}
	var da DeviceAuthorization
	if err := c.post(ctx, "/oidc/device/auth", form, &da); err != nil {
		return nil, err
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return nil, errors.New("logto: incomplete device authorization response")
	}
	da.ExpiresAt = time.Now().Add(da.lifetime())
	return &da, nil
}

// defaultDeviceCodeLifetime is the code lifetime that applies when the
// device authorization response has no positive expires_in.
const defaultDeviceCodeLifetime = 10 * time.Minute

// lifetime returns how long the device code lives after the response.
func (da *DeviceAuthorization) lifetime() time.Duration {
	if da.ExpiresIn <= 0 {
		return defaultDeviceCodeLifetime
	}
	return time.Duration(da.ExpiresIn) * time.Second
}

// PollToken polls the token endpoint until the user signs in, denies the
// request or the device code expires. It waits at least 5 seconds between
// requests (more after slow_down); minInterval overrides that for tests.
// A timeout while the connection to the endpoint is established doubles the
// interval for the later requests (RFC 8628 section 3.5); any other
// transport failure ends the polling.
//
// The local expiry deadline (expires_in seconds after the device
// authorization response, or 10 minutes after it when expires_in is not
// positive; see DeviceAuthorization.ExpiresAt) bounds the wait between requests and every
// token request: PollToken starts no request at or after the deadline, cancels
// a request that is still in flight at the deadline and returns ErrExpired.
// It checks for cancellation and expiry again after processing each response.
// A cancellation or deadline of ctx ends the polling earlier and is returned
// as ctx.Err(), so callers can tell it from the expiry. A code that has
// already expired when PollToken starts returns ErrExpired without any
// request. The deadline applies
// only to the device grant: the returned token does not depend on it, and ctx
// stays usable for the refresh grant afterwards.
func (c Config) PollToken(ctx context.Context, da *DeviceAuthorization, minInterval time.Duration) (*Token, error) {
	interval := time.Duration(da.Interval) * time.Second
	if minInterval == 0 {
		minInterval = 5 * time.Second
	}
	interval = max(interval, minInterval)
	deadline := da.ExpiresAt
	if deadline.IsZero() {
		deadline = time.Now().Add(da.lifetime())
	}
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// ended reports why the polling must stop, or returns err unchanged when
	// it must not. The caller's own cancellation or deadline wins over the
	// expiry; a failure that happens before the deadline is not an expiry.
	ended := func(err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if pollCtx.Err() != nil || !time.Now().Before(deadline) {
			return ErrExpired
		}
		return err
	}
	form := url.Values{
		"client_id":   {c.AppID},
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {da.DeviceCode},
	}
	// A code that expired before the polling started, or a caller context
	// that is already done, sends no request.
	if err := ended(nil); err != nil {
		return nil, err
	}
	for {
		t := time.NewTimer(interval)
		select {
		case <-pollCtx.Done():
			t.Stop()
			return nil, ended(pollCtx.Err())
		case <-t.C:
		}
		// The timer and the context can be ready at the same time.
		if err := ended(nil); err != nil {
			return nil, err
		}
		var tok Token
		err := c.post(pollCtx, "/oidc/token", form, &tok)
		if stop := ended(nil); stop != nil {
			return nil, stop
		}
		if err == nil {
			return &tok, nil
		}
		var oe *Error
		if !errors.As(err, &oe) {
			if !isDialTimeout(err) {
				return nil, err
			}
			// RFC 8628 section 3.5: back off after a connection timeout
			// and keep the longer interval for the later polls.
			if interval <= maxPollInterval/2 {
				interval *= 2
			}
			continue
		}
		switch oe.Code() {
		case authorizationPending:
		case slowDown:
			interval += 5 * time.Second
		case accessDenied:
			return nil, ErrAccessDenied
		case expiredToken:
			return nil, ErrExpired
		default:
			return nil, err
		}
	}
}

// maxPollInterval stops the connection-timeout backoff of PollToken from
// overflowing; the device code expiry ends the polling long before.
const maxPollInterval = time.Duration(math.MaxInt64 / 4)

// isDialTimeout reports whether err is a timeout while the connection to the
// token endpoint was being established. A timeout of a response or of the
// body (Op "read" or the client timeout after the connection was made) is
// not one. The caller must check the caller and device-grant contexts first:
// a dial that ends at their deadline also reports a timeout.
func isDialTimeout(err error) bool {
	var oe *net.OpError
	return errors.As(err, &oe) && oe.Op == "dial" && oe.Timeout()
}

// Refresh exchanges a refresh token for an access token of the configured
// API resource. Logto rotates refresh tokens: store the returned one.
func (c Config) Refresh(ctx context.Context, refreshToken string) (*Token, error) {
	form := url.Values{
		"client_id":     {c.AppID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
	}
	if c.Resource != "" {
		form.Set("resource", c.Resource)
	}
	var tok Token
	if err := c.post(ctx, "/oidc/token", form, &tok); err != nil {
		return nil, err
	}
	if tok.AccessToken == "" {
		return nil, errors.New("logto: the token response has no access token")
	}
	return &tok, nil
}
