// SPDX-License-Identifier: Apache-2.0

// Package logto implements the OAuth 2.0 device authorization flow
// (RFC 8628) and the refresh token grant against a Logto tenant, as the
// ownsand CLI uses them: the CLI is a Logto native application with device
// flow, asks for offline_access in the device authorization and exchanges
// the refresh token for an access token of the OwnSand API resource.
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
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config identifies the tenant, the application and the API resource.
type Config struct {
	// Endpoint is the tenant endpoint, for example https://auth.example.com.
	Endpoint string
	// AppID is the ID of the native application with device flow.
	AppID string
	// Resource is the API resource indicator of the OwnSand API.
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
}

// Token is a token response.
type Token struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

// Error is an OAuth error response. It carries only the error code and the
// description.
type Error struct {
	Code        string `json:"error"`
	Description string `json:"error_description"`
	Status      int    `json:"-"`
}

func (e *Error) Error() string {
	if e.Description != "" {
		return fmt.Sprintf("logto: %s: %s", e.Code, e.Description)
	}
	return "logto: " + e.Code
}

// ErrAccessDenied and ErrExpired end the device flow.
var (
	ErrAccessDenied = errors.New("logto: the sign-in was denied")
	ErrExpired      = errors.New("logto: the device code expired before the sign-in finished")
)

func (c Config) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
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

func (c Config) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path), strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	res, err := c.client().Do(req)
	if err != nil {
		return fmt.Errorf("logto: %s: %w", path, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("logto: %s: %w", path, err)
	}
	if res.StatusCode/100 != 2 {
		e := &Error{Status: res.StatusCode}
		if json.Unmarshal(body, e) != nil || e.Code == "" {
			e.Code = fmt.Sprintf("http_%d", res.StatusCode)
		}
		return e
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("logto: %s: invalid response", path)
	}
	return nil
}

// Validate checks that the configuration names a tenant and an application.
func (c Config) Validate() error {
	if c.Endpoint == "" || c.AppID == "" {
		return errors.New("logto: the tenant endpoint and the application ID are required (OWNSAND_LOGTO_ENDPOINT and OWNSAND_LOGTO_APP_ID)")
	}
	u, err := url.Parse(c.Endpoint)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("logto: the tenant endpoint must be an http or https URL")
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
	return &da, nil
}

// PollToken polls the token endpoint until the user signs in, denies the
// request or the device code expires. It waits at least 5 seconds between
// requests (more after slow_down); minInterval overrides that for tests.
//
// The local expiry deadline (expires_in of the device authorization, or 10
// minutes when it is not positive) bounds the wait between requests and every
// token request: PollToken starts no request at or after the deadline, cancels
// a request that is still in flight at the deadline and returns ErrExpired.
// A cancellation or deadline of ctx ends the polling earlier and is returned
// as ctx.Err(), so callers can tell it from the expiry. The deadline applies
// only to the device grant: the returned token does not depend on it, and ctx
// stays usable for the refresh grant afterwards.
func (c Config) PollToken(ctx context.Context, da *DeviceAuthorization, minInterval time.Duration) (*Token, error) {
	interval := time.Duration(da.Interval) * time.Second
	if minInterval == 0 {
		minInterval = 5 * time.Second
	}
	interval = max(interval, minInterval)
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	if da.ExpiresIn <= 0 {
		deadline = time.Now().Add(10 * time.Minute)
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
		if err == nil {
			return &tok, nil
		}
		var oe *Error
		if !errors.As(err, &oe) {
			return nil, ended(err)
		}
		switch oe.Code {
		case "authorization_pending":
		case "slow_down":
			interval += 5 * time.Second
		case "access_denied":
			return nil, ErrAccessDenied
		case "expired_token":
			return nil, ErrExpired
		default:
			return nil, err
		}
	}
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
