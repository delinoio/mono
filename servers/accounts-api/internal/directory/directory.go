// SPDX-License-Identifier: FSL-1.1-ALv2

// Package directory reads Logto user profiles through the Logto Management
// API with the machine-to-machine credential of accounts-api. It is used only
// for the primary email of the caller of AcceptInvitation and the display
// names of ListMembers (docs/servers-accounts-api-foundation.md,
// Authentication).
package directory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrUserNotFound is returned for a user ID that Logto does not know.
var ErrUserNotFound = errors.New("logto user not found")

// User is the part of a Logto user profile that accounts-api reads.
type User struct {
	// PrimaryEmail is empty when the user has no primary email.
	PrimaryEmail string
	// Name is the display name; empty when the user has none.
	Name string
}

// Directory reads Logto users.
type Directory interface {
	// User returns the profile of the Logto user ID. It returns
	// ErrUserNotFound for an unknown user and another error when Logto is
	// unavailable.
	User(ctx context.Context, userID string) (User, error)
}

// LogtoConfig configures the Management API client.
type LogtoConfig struct {
	// Endpoint is the default Logto endpoint of the tenant,
	// `https://<tenant-id>.logto.app`. The client gets its token from
	// `<Endpoint>/oidc/token` for the resource `<Endpoint>/api` with the scope
	// `all`, and calls `<Endpoint>/api/users/{id}`.
	Endpoint     string
	ClientID     string
	ClientSecret string
	Client       *http.Client
	Now          func() time.Time
	Logger       *slog.Logger
}

// tokenRefreshMargin renews the Management API token this long before it
// expires.
const tokenRefreshMargin = time.Minute

// maxResponseBytes bounds Logto responses.
const maxResponseBytes = 1 << 20

// tokenFetchTimeout bounds one token request. The request is detached from
// the cancellation of the caller that started it, so this timeout is its only
// bound.
const tokenFetchTimeout = 10 * time.Second

// Logto is the Management API client.
type Logto struct {
	cfg LogtoConfig

	// mu guards accessToken and expiresAt. It is held only for memory
	// access, never across a network call.
	mu          sync.Mutex
	accessToken string
	expiresAt   time.Time

	// refreshGate admits one token refresh at a time. It is a channel with
	// capacity one and not a mutex so that a caller waits for it with its
	// context: a caller that is canceled while another refresh runs returns
	// at once.
	refreshGate chan struct{}
}

// NewLogto returns a Management API client. It fetches no token until the
// first call.
func NewLogto(cfg LogtoConfig) *Logto {
	cfg.Endpoint = strings.TrimRight(cfg.Endpoint, "/")
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Logto{cfg: cfg, refreshGate: make(chan struct{}, 1)}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// cachedToken returns the cached access token while it is outside the refresh
// margin.
func (l *Logto) cachedToken() (string, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.accessToken != "" && l.cfg.Now().Before(l.expiresAt.Add(-tokenRefreshMargin)) {
		return l.accessToken, true
	}
	return "", false
}

type tokenResult struct {
	token string
	err   error
}

// token returns a cached Management API access token or fetches a new one.
// Concurrent callers share one refresh. A caller whose context ends while it
// waits for the refresh returns the context error; the refresh itself keeps
// running for the other callers.
func (l *Logto) token(ctx context.Context) (string, error) {
	if token, ok := l.cachedToken(); ok {
		return token, nil
	}
	select {
	case l.refreshGate <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	// Another caller can have refreshed the token while this one waited.
	if token, ok := l.cachedToken(); ok {
		<-l.refreshGate
		return token, nil
	}
	if err := ctx.Err(); err != nil {
		<-l.refreshGate
		return "", err
	}
	// The goroutine owns the gate until the refresh ends, so that a caller
	// that stops waiting cannot let a second refresh start in parallel.
	done := make(chan tokenResult, 1)
	go func() {
		defer func() { <-l.refreshGate }()
		// The refresh serves every waiting caller, so the cancellation of
		// the caller that started it must not abort it; only the timeout
		// bounds it.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), tokenFetchTimeout)
		defer cancel()
		token, err := l.fetchToken(fetchCtx)
		done <- tokenResult{token: token, err: err}
	}()
	select {
	case result := <-done:
		return result.token, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// fetchToken requests a new access token and caches it. It holds no lock
// during the request.
func (l *Logto) fetchToken(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type": {"client_credentials"},
		"resource":   {l.cfg.Endpoint + "/api"},
		"scope":      {"all"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.cfg.Endpoint+"/oidc/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.SetBasicAuth(url.QueryEscape(l.cfg.ClientID), url.QueryEscape(l.cfg.ClientSecret))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := l.cfg.Client.Do(req)
	if err != nil {
		return "", fmt.Errorf("logto token request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// The body can echo request details; it is not logged.
		return "", fmt.Errorf("logto token status %d", resp.StatusCode)
	}
	var body tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil || body.AccessToken == "" {
		return "", errors.New("logto token response is not valid")
	}
	l.mu.Lock()
	l.accessToken = body.AccessToken
	l.expiresAt = l.cfg.Now().Add(time.Duration(body.ExpiresIn) * time.Second)
	l.mu.Unlock()
	l.cfg.Logger.Info("logto_management_token_loaded", slog.Int64("expires_in_s", body.ExpiresIn))
	return body.AccessToken, nil
}

// invalidate drops a token that Logto rejected.
func (l *Logto) invalidate(token string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.accessToken == token {
		l.accessToken = ""
	}
}

type userResponse struct {
	PrimaryEmail *string `json:"primaryEmail"`
	Name         *string `json:"name"`
}

// User implements Directory. A rejected token is renewed once.
func (l *Logto) User(ctx context.Context, userID string) (User, error) {
	if userID == "" {
		return User{}, ErrUserNotFound
	}
	for attempt := 0; ; attempt++ {
		token, err := l.token(ctx)
		if err != nil {
			return User{}, err
		}
		user, status, err := l.getUser(ctx, token, userID)
		if status == http.StatusUnauthorized && attempt == 0 {
			l.invalidate(token)
			continue
		}
		return user, err
	}
}

func (l *Logto) getUser(ctx context.Context, token, userID string) (User, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.cfg.Endpoint+"/api/users/"+url.PathEscape(userID), nil)
	if err != nil {
		return User{}, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := l.cfg.Client.Do(req)
	if err != nil {
		return User{}, 0, fmt.Errorf("logto user request: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return User{}, resp.StatusCode, ErrUserNotFound
	default:
		return User{}, resp.StatusCode, fmt.Errorf("logto user status %d", resp.StatusCode)
	}
	var body userResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil {
		return User{}, resp.StatusCode, errors.New("logto user response is not valid")
	}
	var user User
	if body.PrimaryEmail != nil {
		user.PrimaryEmail = *body.PrimaryEmail
	}
	if body.Name != nil {
		user.Name = *body.Name
	}
	return user, resp.StatusCode, nil
}

// Fake is an in-memory Directory for tests.
type Fake struct {
	mu    sync.Mutex
	users map[string]User
	// Err, when set, is returned by every call.
	Err error
}

// NewFake returns an empty fake directory.
func NewFake() *Fake { return &Fake{users: map[string]User{}} }

// Set stores the profile of a user.
func (f *Fake) Set(userID string, user User) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.users[userID] = user
}

// SetErr makes every call fail with err; nil restores normal answers.
func (f *Fake) SetErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Err = err
}

// User implements Directory.
func (f *Fake) User(_ context.Context, userID string) (User, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Err != nil {
		return User{}, f.Err
	}
	user, ok := f.users[userID]
	if !ok {
		return User{}, ErrUserNotFound
	}
	return user, nil
}
