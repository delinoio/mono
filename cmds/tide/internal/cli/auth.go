// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/cmds/internal/logto"
	"github.com/delinoio/mono/go/tide"
)

// DefaultLogtoResource is the API resource indicator of the tide API.
const DefaultLogtoResource = "https://tide.api.delino.io"

// DefaultLogtoScope is the scope that the tide control plane requires in user
// access tokens. The CLI always requests it.
const DefaultLogtoScope = "tide:use"

// refreshMargin renews an access token this long before it expires.
const refreshMargin = time.Minute

const (
	maxRefreshAttempts = 3
	refreshAdoptWait   = 5 * time.Second
	refreshAdoptPoll   = 50 * time.Millisecond
)

// errNoCredential tells the user how to get a credential.
var errNoCredential = errors.New("not signed in: run 'tide login' or set " + EnvAPIKey)

func (a *app) logtoConfig() logto.Config {
	resource := a.getenv(EnvLogtoResource)
	if resource == "" {
		resource = DefaultLogtoResource
	}
	return logto.Config{
		Endpoint:   a.getenv(EnvLogtoEndpoint),
		AppID:      a.getenv(EnvLogtoAppID),
		Resource:   resource,
		Scopes:     append([]string{DefaultLogtoScope}, strings.Fields(a.getenv(EnvLogtoScopes))...),
		HTTPClient: a.env.HTTPClient,
	}
}

// login runs the Logto device authorization flow and stores the tokens.
func (a *app) login(ctx context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) != 0 {
		return usageErr("usage: tide login")
	}
	if a.getenv(EnvAPIKey) != "" {
		a.warnf("%s is set; commands use the API key and ignore the stored tokens", EnvAPIKey)
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
	// The verification URL and the user code are not secrets; the device
	// code is, and it is never printed.
	url := da.VerificationURIComplete
	if url == "" {
		url = da.VerificationURI
	}
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
	api, err := cfg.Refresh(ctx, tok.RefreshToken)
	if err != nil {
		return err
	}
	sessionID, err := credstore.NewSessionID()
	if err != nil {
		return err
	}
	refresh := api.RefreshToken
	if refresh == "" {
		refresh = tok.RefreshToken
	}
	creds := &credstore.Credentials{
		SessionID:         sessionID,
		LogtoEndpoint:     cfg.Endpoint,
		AppID:             cfg.AppID,
		Resource:          cfg.Resource,
		RefreshToken:      refresh,
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
	a.logger.Debug("tide login stored credentials", "path", st.Path())
	fmt.Fprintln(a.env.Stderr, "Signed in.")
	return nil
}

func (a *app) logout(_ context.Context, name string, args []string) error {
	fs := a.newFlags(name)
	if rest, err := a.parse(fs, args); err != nil {
		return err
	} else if len(rest) != 0 {
		return usageErr("usage: tide logout")
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

// tokenSource returns the stored Logto access token. Before refreshing, it
// reloads credentials to use any rotation that another process has published.
func (a *app) tokenSource() (tide.TokenSource, error) {
	st, err := a.store()
	if err != nil {
		return nil, err
	}
	creds, err := st.Load()
	if errors.Is(err, credstore.ErrNotFound) {
		return nil, errNoCredential
	}
	if err != nil {
		return nil, err
	}
	// The tokens belong to the tenant, the application and the resource
	// that issued them; a configuration that names others must not use
	// them.
	if name, ok := credentialMismatch(creds, a.getenv(EnvLogtoEndpoint), a.getenv(EnvLogtoAppID), a.getenv(EnvLogtoResource)); ok {
		return nil, fmt.Errorf("the stored credentials were issued for a different %s value; run 'tide login' again", name)
	}
	cfg := a.logtoConfig()
	cfg.Endpoint, cfg.AppID = creds.LogtoEndpoint, creds.AppID
	if creds.Resource != "" {
		cfg.Resource = creds.Resource
	}
	refreshLock := make(chan struct{}, 1)
	var staleErr error
	fresh := func() bool {
		return creds.AccessToken != "" && time.Until(creds.AccessTokenExpiry) > refreshMargin
	}
	// A removed or replaced session makes this source permanently stale.
	adopt := func() (bool, error) {
		stored, err := st.Load()
		switch {
		case errors.Is(err, credstore.ErrNotFound):
			staleErr = errors.New("the stored credentials were removed (signed out); run 'tide login' to sign in again")
			return false, staleErr
		case err != nil:
			return false, err
		case stored.SessionID != creds.SessionID:
			staleErr = errors.New("the stored credentials were replaced (signed in again); run the command again to load them")
			return false, staleErr
		}
		rotated := stored.RefreshToken != creds.RefreshToken
		creds = stored
		return rotated, nil
	}
	return func(ctx context.Context) (string, error) {
		select {
		case refreshLock <- struct{}{}:
			defer func() { <-refreshLock }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if staleErr != nil {
			return "", staleErr
		}
		if fresh() {
			return creds.AccessToken, nil
		}
		if _, err := adopt(); err != nil {
			return "", err
		}
		for attempt := 1; ; attempt++ {
			if fresh() {
				a.logger.Debug("tide adopted the Logto access token that another process stored")
				return creds.AccessToken, nil
			}
			if attempt > maxRefreshAttempts {
				return "", errors.New("the Logto token was refreshed by another process too often while this command refreshed it; run the command again")
			}
			sent := creds.RefreshToken
			tok, err := cfg.Refresh(ctx, sent)
			if err != nil {
				var oe *logto.Error
				if attempt < maxRefreshAttempts && errors.As(err, &oe) && oe.Code == "invalid_grant" {
					rotated, adoptErr := adopt()
					if adoptErr != nil {
						return "", adoptErr
					}
					if !rotated {
						// The issuer may reject the spent token before the
						// other process has published its replacement.
						a.logger.Debug("tide waits for another process to publish the rotated Logto refresh token")
						rotated, adoptErr = awaitRefreshRotation(ctx, adopt)
						if adoptErr != nil {
							return "", adoptErr
						}
					}
					if rotated {
						continue
					}
				}
				return "", fmt.Errorf("refresh the Logto token (run 'tide login' again if it was revoked): %w", err)
			}
			next := *creds
			next.AccessToken = tok.AccessToken
			next.AccessTokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
			if tok.RefreshToken != "" {
				next.RefreshToken = tok.RefreshToken
			}
			switch err := st.SaveIfCurrent(creds.SessionID, sent, &next); {
			case err == nil:
				creds = &next
				a.logger.Debug("tide refreshed the Logto access token")
				return creds.AccessToken, nil
			case errors.Is(err, credstore.ErrNotFound):
				staleErr = errors.New("the stored credentials were removed while the Logto token refreshed (signed out); run 'tide login' to sign in again")
				return "", staleErr
			case errors.Is(err, credstore.ErrReplaced):
				staleErr = errors.New("the stored credentials were replaced while the Logto token refreshed (signed in again); run the command again to load them")
				return "", staleErr
			case errors.Is(err, credstore.ErrRotated):
				a.logger.Debug("tide discarded a refreshed Logto token after another process stored a newer one")
				if _, err := adopt(); err != nil {
					return "", err
				}
			default:
				return "", err
			}
		}
	}, nil
}

func awaitRefreshRotation(ctx context.Context, adopt func() (bool, error)) (bool, error) {
	deadline := time.NewTimer(refreshAdoptWait)
	defer deadline.Stop()
	tick := time.NewTicker(refreshAdoptPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, fmt.Errorf("wait for the rotated Logto refresh token: %w", ctx.Err())
		case <-deadline.C:
			return adopt()
		case <-tick.C:
			if rotated, err := adopt(); err != nil || rotated {
				return rotated, err
			}
		}
	}
}

// credentialMismatch reports the first configured Logto setting that
// differs from the identity of the stored credentials.
func credentialMismatch(creds *credstore.Credentials, endpoint, appID, resource string) (string, bool) {
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

// callerUserID returns the Logto user ID (sub) of the current access token.
// It reads the token payload without verifying it: the server verifies the
// token, and the CLI only needs the ID to name itself in RemoveMember.
func (a *app) callerUserID(ctx context.Context) (string, error) {
	if a.getenv(EnvAPIKey) != "" {
		return "", errors.New("leaving an organization needs the stored Logto tokens of 'tide login'; a personal API key works only for 'tide session' and 'tide mcp'")
	}
	// Use the token source of the client, so that a refresh here is not
	// repeated by the client with the old refresh token.
	if _, err := a.sdk(); err != nil {
		return "", err
	}
	tok, err := a.tokens(ctx)
	if err != nil {
		return "", err
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return "", errors.New("the access token is not a JWT; cannot read the user ID")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", errors.New("the access token payload is not valid")
	}
	var claims struct {
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Sub == "" {
		return "", errors.New("the access token has no user ID")
	}
	return claims.Sub, nil
}
