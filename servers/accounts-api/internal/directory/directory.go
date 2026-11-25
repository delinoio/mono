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

	// inflight is the token refresh that is running, or nil. It is guarded
	// by mu. Callers that need a token while it is set join it and share its
	// result, also when it fails, so that a burst of lookups sends one token
	// request and not one request per caller.
	inflight *tokenRefresh
}

// tokenRefresh is one token request that every caller of a burst shares.
type tokenRefresh struct {
	// done is closed after token and err are set.
	done  chan struct{}
	token string
	err   error
	// waiters counts the callers that started or joined the refresh. It is
	// guarded by Logto.mu and read by tests only.
	waiters int
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
	return &Logto{cfg: cfg}
}

type tokenResponse struct {
	AccessToken string `json:"access_token"`
	ExpiresIn   int64  `json:"expires_in"`
}

// token returns a cached Management API access token or fetches a new one.
// Concurrent callers share one refresh and its result, the error of a failed
// refresh included. A caller whose context ends while it waits for the
// refresh returns the context error; the refresh itself keeps running for the
// other callers. A caller that needs a token after a failed refresh has ended
// starts a new one.
func (l *Logto) token(ctx context.Context) (string, error) {
	l.mu.Lock()
	if l.accessToken != "" && l.cfg.Now().Before(l.expiresAt.Add(-tokenRefreshMargin)) {
		token := l.accessToken
		l.mu.Unlock()
		return token, nil
	}
	refresh := l.inflight
	if refresh == nil {
		if err := ctx.Err(); err != nil {
			l.mu.Unlock()
			return "", err
		}
		refresh = &tokenRefresh{done: make(chan struct{})}
		l.inflight = refresh
		go l.runRefresh(context.WithoutCancel(ctx), refresh)
	}
	refresh.waiters++
	l.mu.Unlock()

	select {
	case <-refresh.done:
		return refresh.token, refresh.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// runRefresh fetches a token for every caller that waits on refresh and
// publishes the result. The refresh serves every waiting caller, so the
// cancellation of the caller that started it must not abort it (the caller
// passes a context detached from its cancellation); only the timeout bounds
// it.
func (l *Logto) runRefresh(ctx context.Context, refresh *tokenRefresh) {
	ctx, cancel := context.WithTimeout(ctx, tokenFetchTimeout)
	defer cancel()
	token, err := l.fetchToken(ctx)
	l.mu.Lock()
	refresh.token, refresh.err = token, err
	l.inflight = nil
	l.mu.Unlock()
	close(refresh.done)
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

// User implements Directory. It evicts rejected tokens and retries once.
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
		if status == http.StatusUnauthorized {
			l.invalidate(token)
			if attempt == 0 {
				continue
			}
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
	var body *userResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(&body); err != nil || body == nil {
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
	// onUser, when set, runs at the start of every User call, outside the
	// lock. Tests use it to move time or hold locks during a lookup.
	onUser func()
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

// SetOnUser registers fn to run at the start of every User call; nil removes
// it. It lets tests change the world while a lookup is in flight.
func (f *Fake) SetOnUser(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onUser = fn
}

// User implements Directory.
func (f *Fake) User(_ context.Context, userID string) (User, error) {
	f.mu.Lock()
	hook := f.onUser
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
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
