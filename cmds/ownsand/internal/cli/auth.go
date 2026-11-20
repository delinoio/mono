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
	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
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
		Scopes:     strings.Fields(a.getenv(EnvLogtoScopes)),
		HTTPClient: a.env.HTTPClient,
	}
}

// login runs the Logto device authorization flow and stores the tokens.
func (a *app) login(ctx context.Context, args []string) error {
	fs := a.newFlags("login")
	if _, err := a.parse(fs, args); err != nil {
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
	creds := &credstore.Credentials{
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
	if _, err := a.parse(fs, args); err != nil {
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
	if cfg.Endpoint == "" {
		cfg.Endpoint = creds.LogtoEndpoint
	}
	if cfg.AppID == "" {
		cfg.AppID = creds.AppID
	}
	if creds.Resource != "" {
		cfg.Resource = creds.Resource
	}
	var mu sync.Mutex
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if creds.AccessToken != "" && time.Until(creds.AccessTokenExpiry) > refreshMargin {
			return creds.AccessToken, nil
		}
		tok, err := cfg.Refresh(ctx, creds.RefreshToken)
		if err != nil {
			return "", fmt.Errorf("refresh the Logto token (run 'ownsand login' again if it was revoked): %w", err)
		}
		creds.AccessToken = tok.AccessToken
		creds.AccessTokenExpiry = time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		if tok.RefreshToken != "" {
			creds.RefreshToken = tok.RefreshToken
		}
		if err := st.Save(creds); err != nil {
			return "", err
		}
		a.logger.Debug("ownsand refreshed the Logto access token")
		return creds.AccessToken, nil
	}, nil
}
