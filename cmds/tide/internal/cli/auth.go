// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/internal/credstore"
	"github.com/delinoio/mono/cmds/internal/logto"
	"github.com/delinoio/mono/go/tide"
)

// DefaultLogtoResource is the API resource indicator of the tide API.
const DefaultLogtoResource = "https://tide.api.delino.io"

// refreshMargin renews an access token this long before it expires.
const refreshMargin = time.Minute

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
		Scopes:     strings.Fields(a.getenv(EnvLogtoScopes)),
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
	fmt.Fprintf(a.env.Stderr, "Open %s in a browser and enter the code %s to sign in.\n", a.bold(url), a.bold(da.UserCode))
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

// tokenSource returns the Logto access token of the stored credentials and
// refreshes it shortly before it expires, storing the rotated refresh
// token.
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
	var mu sync.Mutex
	var staleErr error
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if staleErr != nil {
			return "", staleErr
		}
		if creds.AccessToken != "" && time.Until(creds.AccessTokenExpiry) > refreshMargin {
			return creds.AccessToken, nil
		}
		tok, err := cfg.Refresh(ctx, creds.RefreshToken)
		if err != nil {
			return "", fmt.Errorf("refresh the Logto token (run 'tide login' again if it was revoked): %w", err)
		}
		next := *creds
		next.AccessToken = tok.AccessToken
		next.AccessTokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		if tok.RefreshToken != "" {
			next.RefreshToken = tok.RefreshToken
		}
		switch err := st.SaveIfSession(creds.SessionID, &next); {
		case err == nil:
		case errors.Is(err, credstore.ErrNotFound):
			staleErr = errors.New("the stored credentials were removed while the Logto token refreshed (signed out); run 'tide login' to sign in again")
			return "", staleErr
		case errors.Is(err, credstore.ErrReplaced):
			staleErr = errors.New("the stored credentials were replaced while the Logto token refreshed (signed in again); run the command again to load them")
			return "", staleErr
		default:
			return "", err
		}
		*creds = next
		a.logger.Debug("tide refreshed the Logto access token")
		return creds.AccessToken, nil
	}, nil
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
	src, err := a.tokenSource()
	if err != nil {
		return "", err
	}
	tok, err := src(ctx)
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
