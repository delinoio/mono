// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/cmds/ownsand/internal/credstore"
	"github.com/delinoio/mono/cmds/ownsand/internal/logto"
	ownsand "github.com/delinoio/mono/go/ownsand"
)

// DefaultLogtoResource is the API resource indicator of the OwnSand API.
const DefaultLogtoResource = "https://ownsand.api.delino.io"

// refreshMargin renews an access token this long before it expires.
const refreshMargin = time.Minute

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
	fmt.Fprintf(a.env.Stderr, "Open %s in a browser and enter the code %s to sign in.\n", a.bold(url), a.bold(da.UserCode))
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
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if staleErr != nil {
			return "", staleErr
		}
		if creds.AccessToken != "" && time.Until(creds.AccessTokenExpiry) > refreshMargin {
			return creds.AccessToken, nil
		}
		// The network request runs without the store lock. The stored
		// session is compared under the lock when the result is published.
		tok, err := cfg.Refresh(ctx, creds.RefreshToken)
		if err != nil {
			return "", fmt.Errorf("refresh the Logto token (run 'ownsand login' again if it was revoked): %w", err)
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
			// Do not recreate the file: the user signed out while the
			// refresh was in flight.
			a.logger.Debug("ownsand discarded a refreshed Logto token: the stored credentials were removed")
			staleErr = errors.New("the stored credentials were removed while the Logto token refreshed (signed out); run 'ownsand login' to sign in again")
			return "", staleErr
		case errors.Is(err, credstore.ErrReplaced):
			a.logger.Debug("ownsand discarded a refreshed Logto token: the stored credentials were replaced")
			staleErr = errors.New("the stored credentials were replaced while the Logto token refreshed (signed in again); run the command again to load them")
			return "", staleErr
		default:
			return "", err
		}
		*creds = next
		a.logger.Debug("ownsand refreshed the Logto access token")
		return creds.AccessToken, nil
	}, nil
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
