// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/cmds/internal/logto"
	ownsand "github.com/delinoio/mono/go/ownsand"
)

// DefaultLogtoResource is the API resource indicator of the OwnSand API.
const DefaultLogtoResource = "https://ownsand.api.delino.io"

// refreshMargin renews an access token this long before it expires.
const refreshMargin = time.Minute

// maxRefreshAttempts bounds the refresh requests of one token request when
// other processes keep rotating the stored refresh token meanwhile.
const maxRefreshAttempts = 3

var errRefreshContention = errors.New("the Logto token was refreshed by another process too often while this command refreshed it; run the command again")

// defaultRefreshAdoptWait bounds how long a token request waits for another
// process to publish the rotated refresh token after the token endpoint
// rejected the one that this process sent with invalid_grant.
const defaultRefreshAdoptWait = 5 * time.Second

// defaultRefreshAdoptPoll is how often that wait reloads the stored
// credentials.
const defaultRefreshAdoptPoll = 50 * time.Millisecond

func (a *app) logtoConfig() logto.Config {
	resource := a.getenv(EnvLogtoResource)
	if resource == "" {
		resource = DefaultLogtoResource
	}
	return logto.Config{
		Endpoint:   a.getenv(EnvLogtoEndpoint),
		AppID:      a.getenv(EnvLogtoAppID),
		Resource:   resource,
		Scopes:     logtoScopes(a.getenv(EnvLogtoScopes)),
		HTTPClient: a.env.HTTPClient,
	}
}

// RequiredLogtoScope is the API resource scope that the control plane
// requires in every user access token.
const RequiredLogtoScope = "ownsand:use"

// logtoScopes returns the API resource scopes that the CLI requests: always
// RequiredLogtoScope, followed by the space-separated extra scopes of
// OWNSAND_LOGTO_SCOPES. Without the required scope the control plane rejects
// every token of the device flow.
func logtoScopes(extra string) []string {
	return append([]string{RequiredLogtoScope}, strings.Fields(extra)...)
}

// login runs the Logto device authorization flow and stores the tokens.
func (a *app) login(ctx context.Context, args []string) error {
	fs := a.newFlags("login")
	// login takes no operands: reject any before the sign-in starts.
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	if a.getenv(ownsand.EnvAPIKey) != "" {
		a.warnf("%s is set; commands use the API key and ignore the stored tokens", ownsand.EnvAPIKey)
	}
	cfg := a.logtoConfig()
	if err := cfg.Validate(); err != nil {
		if errors.Is(err, logto.ErrMissingConfig) {
			return fmt.Errorf("%w (%s and %s)", err, EnvLogtoEndpoint, EnvLogtoAppID)
		}
		return err
	}
	da, err := cfg.StartDeviceAuthorization(ctx)
	if err != nil {
		return err
	}
	// The verification URL and the user code are not secrets of the
	// session; the device code is, and it is never printed.
	url := da.VerificationURIComplete
	if url == "" {
		url = da.VerificationURI
	}
	// Without the instructions the user cannot complete the sign-in, so a
	// failed write (a closed pipe or a full disk) ends the login before any
	// token polling instead of waiting for a code that nobody can enter.
	if _, err := fmt.Fprintf(a.env.Stderr, "Open %s in a browser and enter the code %s to sign in.\n", a.bold(url), a.bold(da.UserCode)); err != nil {
		return fmt.Errorf("write the sign-in instructions: %w", err)
	}
	tok, err := cfg.PollToken(ctx, da, a.env.LoginPollInterval)
	if err != nil {
		return err
	}
	if tok.RefreshToken == "" {
		return errors.New("logto returned no refresh token; the application must allow offline_access")
	}
	// Exchange the refresh token for an access token of the API resource.
	api, err := cfg.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return err
	}
	sessionID, err := credstore.NewSessionID()
	if err != nil {
		return err
	}
	creds := &credstore.Credentials{
		SessionID:         sessionID,
		LogtoEndpoint:     cfg.Endpoint,
		AppID:             cfg.AppID,
		Resource:          cfg.Resource,
		RefreshToken:      firstNonEmpty(api.RefreshToken, tok.RefreshToken),
		AccessToken:       api.AccessToken,
		AccessTokenExpiry: time.Now().Add(time.Duration(api.ExpiresIn) * time.Second),
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	if err := st.Save(creds); err != nil {
		return err
	}
	a.logger.Debug("ownsand login stored credentials", "path", st.Path())
	fmt.Fprintln(a.env.Stderr, "Signed in.")
	return nil
}

func (a *app) logout(args []string) error {
	fs := a.newFlags("logout")
	// logout takes no operands: reject any before the store is touched.
	if _, err := a.positional(fs, args); err != nil {
		return err
	}
	st, err := a.store()
	if err != nil {
		return err
	}
	if err := st.Delete(); err != nil {
		return err
	}
	fmt.Fprintln(a.env.Stderr, "Signed out.")
	return nil
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}

// tokenSource returns the Logto access token of the stored credentials and
// refreshes it shortly before it expires, storing the rotated refresh token.
// Before it refreshes, it reloads the stored credentials, so a refresh that
// another process finished is used instead of repeated; it publishes the result
// only over the refresh token it sent.
func (a *app) tokenSource() (ownsand.TokenSource, error) {
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	creds, err := st.Load()
	if errors.Is(err, credstore.ErrNotFound) {
		return nil, fmt.Errorf("not signed in: run 'ownsand login' or set %s", ownsand.EnvAPIKey)
	}
	if err != nil {
		return nil, err
	}
	cfg := a.logtoConfig()
	// The tokens belong to the tenant, the application and the resource
	// that issued them. A configuration that names others must not use
	// them: the cached access token would fail, and the refresh token would
	// go to another tenant's token endpoint.
	if name, ok := credentialMismatch(creds, a.getenv(EnvLogtoEndpoint), a.getenv(EnvLogtoAppID), a.getenv(EnvLogtoResource)); ok {
		return nil, fmt.Errorf("the stored credentials were issued for a different %s value; run 'ownsand login' again", name)
	}
	cfg.Endpoint, cfg.AppID = creds.LogtoEndpoint, creds.AppID
	if creds.Resource != "" {
		cfg.Resource = creds.Resource
	}
	var mu sync.Mutex
	// staleErr is set once the stored session was cleared or replaced; the
	// source then refuses to use or publish the tokens it holds.
	var staleErr error
	// fresh reports whether the access token needs no refresh yet.
	fresh := func() bool {
		return creds.AccessToken != "" && time.Until(creds.AccessTokenExpiry) > refreshMargin
	}
	// adopt replaces the cached credentials with the stored ones of the same
	// session, which another process may have refreshed. It reports whether
	// the stored refresh token differs from the cached one. A removed or
	// replaced session makes the source stale.
	adopt := func() (bool, error) {
		stored, err := st.Load()
		switch {
		case errors.Is(err, credstore.ErrNotFound):
			a.logger.Debug("ownsand found the stored credentials removed before a Logto token refresh")
			staleErr = errors.New("the stored credentials were removed (signed out); run 'ownsand login' to sign in again")
			return false, staleErr
		case err != nil:
			return false, err
		case stored.SessionID != creds.SessionID:
			a.logger.Debug("ownsand found the stored credentials replaced before a Logto token refresh")
			staleErr = errors.New("the stored credentials were replaced (signed in again); run the command again to load them")
			return false, staleErr
		}
		rotated := stored.RefreshToken != creds.RefreshToken
		*creds = *stored
		return rotated, nil
	}
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if staleErr != nil {
			return "", staleErr
		}
		for attempt := 1; ; attempt++ {
			if fresh() {
				return creds.AccessToken, nil
			}
			// Another process may have refreshed the tokens since this
			// source loaded them. Logto rotates the refresh token, so the
			// cached one may be spent: use the stored generation first.
			if _, err := adopt(); err != nil {
				return "", err
			}
			if fresh() {
				a.logger.Debug("ownsand adopted the Logto access token that another process refreshed")
				return creds.AccessToken, nil
			}
			// The network request runs without the store lock. The stored
			// generation is compared under the lock when the result is
			// published.
			sent := creds.RefreshToken
			tok, err := cfg.Refresh(ctx, sent)
			if err != nil {
				// Another process may have spent this refresh token while
				// the request was in flight.
				var oe *logto.Error
				if errors.As(err, &oe) && oe.Code() == logto.InvalidGrant {
					rotated, aerr := adopt()
					if aerr != nil {
						return "", aerr
					}
					if !rotated {
						// The refresh in another process may have rotated
						// the token without publishing the result yet.
						if rotated, aerr = a.awaitRefreshRotation(ctx, adopt); aerr != nil {
							return "", aerr
						}
					}
					if rotated {
						if fresh() {
							a.logger.Debug("ownsand adopted the Logto access token that another process refreshed")
							return creds.AccessToken, nil
						}
						if attempt >= maxRefreshAttempts {
							return "", errRefreshContention
						}
						a.logger.Debug("ownsand retries the Logto token refresh with the token that another process stored")
						continue
					}
				}
				return "", fmt.Errorf("refresh the Logto token (run 'ownsand login' again if it was revoked): %w", err)
			}
			next := *creds
			next.AccessToken = tok.AccessToken
			next.AccessTokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
			if tok.RefreshToken != "" {
				next.RefreshToken = tok.RefreshToken
			}
			switch err := st.SaveIfCurrent(creds.SessionID, sent, &next); {
			case err == nil:
			case errors.Is(err, credstore.ErrNotFound):
				// Do not recreate the file: the user signed out while the
				// refresh was in flight.
				a.logger.Debug("ownsand discarded a refreshed Logto token: the stored credentials were removed")
				staleErr = errors.New("the stored credentials were removed while the Logto token refreshed (signed out); run 'ownsand login' to sign in again")
				return "", staleErr
			case errors.Is(err, credstore.ErrReplaced):
				a.logger.Debug("ownsand discarded a refreshed Logto token: the stored credentials were replaced")
				staleErr = errors.New("the stored credentials were replaced while the Logto token refreshed (signed in again); run the command again to load them")
				return "", staleErr
			case errors.Is(err, credstore.ErrRotated):
				// Another process stored a newer rotation of the same
				// session. Keep it: this result derives from an older
				// refresh token and must not replace it.
				a.logger.Debug("ownsand discarded a refreshed Logto token: another process stored a newer one")
				if _, aerr := adopt(); aerr != nil {
					return "", aerr
				}
				if fresh() {
					return creds.AccessToken, nil
				}
				if attempt >= maxRefreshAttempts {
					return "", errRefreshContention
				}
				continue
			default:
				return "", err
			}
			*creds = next
			a.logger.Debug("ownsand refreshed the Logto access token")
			return creds.AccessToken, nil
		}
	}, nil
}

// awaitRefreshRotation polls for a newer credential generation in the same
// session. Another process can spend the refresh token before publishing its
// replacement. Check the store at the deadline to catch publications after
// the final poll.
func (a *app) awaitRefreshRotation(ctx context.Context, adopt func() (bool, error)) (bool, error) {
	wait, poll := a.env.RefreshAdoptWait, a.env.RefreshAdoptPoll
	if wait <= 0 {
		wait = defaultRefreshAdoptWait
	}
	if poll <= 0 {
		poll = defaultRefreshAdoptPoll
	}
	a.logger.Debug("ownsand waits for another process to publish the rotated Logto refresh token",
		slog.Duration("wait", wait), slog.Duration("poll", poll))
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	tick := time.NewTicker(poll)
	defer tick.Stop()
	for {
		reachedDeadline := false
		select {
		case <-ctx.Done():
		case <-deadline.C:
			reachedDeadline = true
		case <-tick.C:
		}
		if err := ctx.Err(); err != nil {
			return false, fmt.Errorf("wait for the rotated Logto refresh token: %w", err)
		}
		if rotated, err := adopt(); reachedDeadline || rotated || err != nil {
			return rotated, err
		}
	}
}

// credentialMismatch reports the first explicitly configured Logto setting
// (empty values are not configured) that differs from the identity of the
// stored credentials.
func credentialMismatch(creds *credstore.Credentials, endpoint, appID, resource string) (string, bool) {
	// The endpoint is compared as the CLI uses it, without trailing slashes.
	normalize := func(u string) string { return strings.TrimRight(u, "/") }
	switch {
	case endpoint != "" && normalize(endpoint) != normalize(creds.LogtoEndpoint):
		return EnvLogtoEndpoint, true
	case appID != "" && appID != creds.AppID:
		return EnvLogtoAppID, true
	case resource != "" && creds.Resource != "" && resource != creds.Resource:
		return EnvLogtoResource, true
	}
	return "", false
}
