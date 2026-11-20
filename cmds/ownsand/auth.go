package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	ownsand "github.com/delinoio/mono/packages/ownsand-sdk-go"
)

// Logto configuration of "ownsand login". The CLI's Logto native
// application (with device flow) is not registered yet, so there is no
// built-in default: the endpoint and the application ID come from flags or
// these environment variables.
const (
	envLogtoEndpoint = "OWNSAND_LOGTO_ENDPOINT"
	envLogtoAppID    = "OWNSAND_LOGTO_APP_ID"
	envLogtoScopes   = "OWNSAND_LOGTO_SCOPES"
	// defaultScopes asks for a refresh token. The OwnSand API scope names are
	// recorded by the control plane with its token validation; add them with
	// --scope or OWNSAND_LOGTO_SCOPES until then.
	defaultScopes = "openid offline_access"
	// credentialFile is the file name in the configuration directory.
	credentialFile = "credentials.json"
	// refreshMargin renews an access token this long before it expires.
	refreshMargin = 30 * time.Second
	deviceGrant   = "urn:ietf:params:oauth:grant-type:device_code"
)

// storedCredential is the content of the credential file.
type storedCredential struct {
	LogtoEndpoint string    `json:"logto_endpoint"`
	AppID         string    `json:"app_id"`
	Resource      string    `json:"resource"`
	AccessToken   string    `json:"access_token"`
	RefreshToken  string    `json:"refresh_token,omitempty"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// pollSleep waits between device flow polls; tests shorten it.
var pollSleep = func(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (a *app) configDir() (string, error) {
	if d := a.getenv(envConfigDir); d != "" {
		return d, nil
	}
	d, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find the user configuration directory: %w", err)
	}
	return filepath.Join(d, "ownsand"), nil
}

func (a *app) credentialPath() (string, error) {
	d, err := a.configDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, credentialFile), nil
}

func (a *app) loadCredential() (*storedCredential, string, error) {
	p, err := a.credentialPath()
	if err != nil {
		return nil, "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, p, err
	}
	var c storedCredential
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, p, fmt.Errorf("read %s: %w", p, err)
	}
	return &c, p, nil
}

// writeCredential writes the credential file so that only the user can
// read it: mode 0600 in a 0700 directory, and a user-only ACL on Windows.
func writeCredential(path string, c *storedCredential) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if err := restrictToUser(dir, true); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil && !errors.Is(err, errChmodUnsupported) {
		f.Close()
		return err
	}
	if err := restrictToUser(tmp, false); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return restrictToUser(path, false)
}

// client returns an SDK client. OWNSAND_API_KEY takes precedence over the
// stored Logto tokens.
func (a *app) client() (*ownsand.Client, error) {
	origin := a.g.origin
	if origin == "" {
		origin = a.getenv(envOrigin)
	}
	opts := ownsand.Options{Origin: origin, OrganizationID: a.g.org, Logger: a.log, Clock: a.clk}
	if opts.OrganizationID == "" {
		opts.OrganizationID = a.getenv(ownsand.EnvOrganization)
	}
	switch a.g.transport {
	case "auto":
	case "relay":
		opts.Transport = ownsand.TransportRelay
	case "peer-to-peer":
		opts.Transport = ownsand.TransportPeerToPeer
	default:
		return nil, usagef("--transport must be auto, relay or peer-to-peer")
	}
	if key := a.getenv(ownsand.EnvAPIKey); key != "" {
		opts.APIKey = key
		a.log.Debug("using the API key from the environment")
	} else {
		cred, path, err := a.loadCredential()
		if errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("not signed in: run \"ownsand login\" or set OWNSAND_API_KEY")
		}
		if err != nil {
			return nil, err
		}
		opts.Token = a.tokenSource(cred, path)
		a.log.Debug("using the stored Logto tokens")
	}
	return ownsand.New(opts)
}

func (a *app) orgClient() (*ownsand.Client, []ownsand.CallOption, error) {
	c, err := a.client()
	if err != nil {
		return nil, nil, err
	}
	if c.OrganizationID() == "" {
		return nil, nil, usagef("no organization: pass --org or set OWNSAND_ORG")
	}
	return c, nil, nil
}

func (a *app) sandbox(id string) (*ownsand.Sandbox, error) {
	c, _, err := a.orgClient()
	if err != nil {
		return nil, err
	}
	return c.Sandbox(id)
}

// tokenSource returns the stored access token and refreshes it shortly
// before it expires. Logto rotates refresh tokens, so every refresh is
// written back to the credential file.
func (a *app) tokenSource(cred *storedCredential, path string) ownsand.TokenFunc {
	var mu sync.Mutex
	return func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if a.now().Add(refreshMargin).Before(cred.ExpiresAt) {
			return cred.AccessToken, nil
		}
		if cred.RefreshToken == "" {
			return "", errors.New("the stored token expired: run \"ownsand login\" again")
		}
		tok, err := a.tokenRequest(ctx, cred.LogtoEndpoint, url.Values{
			"grant_type":    {"refresh_token"},
			"client_id":     {cred.AppID},
			"refresh_token": {cred.RefreshToken},
			"resource":      {cred.Resource},
		})
		if err != nil {
			return "", fmt.Errorf("refresh the Logto token (run \"ownsand login\" again if this persists): %w", err)
		}
		cred.AccessToken = tok.AccessToken
		if tok.RefreshToken != "" {
			cred.RefreshToken = tok.RefreshToken
		}
		cred.ExpiresAt = a.now().Add(time.Duration(tok.ExpiresIn) * time.Second)
		if err := writeCredential(path, cred); err != nil {
			return "", fmt.Errorf("store the refreshed token: %w", err)
		}
		a.log.Debug("refreshed the Logto access token")
		return cred.AccessToken, nil
	}
}

type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"`
	Error        string `json:"error"`
	Description  string `json:"error_description"`
}

// oauthError is an OAuth error response; its text never holds a token.
type oauthError struct{ code, desc string }

func (e *oauthError) Error() string {
	if e.desc != "" {
		return e.code + ": " + e.desc
	}
	return e.code
}

var logtoHTTP = &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func (a *app) postForm(ctx context.Context, endpoint string, form url.Values, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := logtoHTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, v); err != nil {
		return fmt.Errorf("Logto returned HTTP %d without a JSON body", resp.StatusCode)
	}
	return nil
}

func (a *app) tokenRequest(ctx context.Context, endpoint string, form url.Values) (*tokenResponse, error) {
	var tok tokenResponse
	if err := a.postForm(ctx, endpoint+"/oidc/token", form, &tok); err != nil {
		return nil, err
	}
	if tok.Error != "" {
		return nil, &oauthError{code: tok.Error, desc: tok.Description}
	}
	if tok.AccessToken == "" {
		return nil, errors.New("Logto returned no access token")
	}
	return &tok, nil
}

func checkLogtoEndpoint(s string) (string, error) {
	u, err := url.Parse(strings.TrimSuffix(s, "/"))
	if err != nil || u.Host == "" {
		return "", usagef("invalid Logto endpoint %q", s)
	}
	host := u.Hostname()
	loopback := host == "localhost" || host == "127.0.0.1" || host == "::1"
	if u.Scheme != "https" && !(u.Scheme == "http" && loopback) {
		return "", usagef("the Logto endpoint must use https")
	}
	return u.String(), nil
}

func loginCommand() command {
	var endpoint, appID, scopes string
	return command{
		usage: "[--logto-endpoint URL] [--app-id ID] [--scope SCOPES]",
		flags: func(fs *flag.FlagSet) {
			fs.StringVar(&endpoint, "logto-endpoint", "", "Logto tenant endpoint (default $OWNSAND_LOGTO_ENDPOINT)")
			fs.StringVar(&appID, "app-id", "", "Logto application ID of the CLI (default $OWNSAND_LOGTO_APP_ID)")
			fs.StringVar(&scopes, "scope", "", "space-separated scopes (default $OWNSAND_LOGTO_SCOPES or \""+defaultScopes+"\")")
		},
		run: func(a *app, fs *flag.FlagSet, args []string) error {
			if len(args) != 0 {
				return usagef("login takes no arguments")
			}
			if endpoint == "" {
				endpoint = a.getenv(envLogtoEndpoint)
			}
			if appID == "" {
				appID = a.getenv(envLogtoAppID)
			}
			if scopes == "" {
				scopes = a.getenv(envLogtoScopes)
			}
			if scopes == "" {
				scopes = defaultScopes
			}
			if endpoint == "" || appID == "" {
				return usagef("the Logto endpoint and application ID are required: pass --logto-endpoint and --app-id or set %s and %s", envLogtoEndpoint, envLogtoAppID)
			}
			ep, err := checkLogtoEndpoint(endpoint)
			if err != nil {
				return err
			}
			resource := a.g.origin
			if resource == "" {
				resource = a.getenv(envOrigin)
			}
			if resource == "" {
				resource = ownsand.DefaultOrigin
			}
			return a.login(ep, appID, scopes, strings.TrimSuffix(resource, "/"))
		},
	}
}

type deviceAuthResponse struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
	Error                   string `json:"error"`
	Description             string `json:"error_description"`
}

// login runs the OAuth 2.0 device authorization flow (RFC 8628) against
// the Logto tenant and stores the tokens.
func (a *app) login(endpoint, appID, scopes, resource string) error {
	var da deviceAuthResponse
	if err := a.postForm(a.ctx, endpoint+"/oidc/device/auth", url.Values{
		"client_id": {appID}, "scope": {scopes}, "resource": {resource},
	}, &da); err != nil {
		return fmt.Errorf("start the device authorization: %w", err)
	}
	if da.Error != "" {
		return fmt.Errorf("start the device authorization: %w", &oauthError{code: da.Error, desc: da.Description})
	}
	if da.DeviceCode == "" || da.UserCode == "" || da.VerificationURI == "" {
		return errors.New("Logto returned an incomplete device authorization response")
	}
	fmt.Fprintf(a.io.err, "Open %s and enter the code %s\n", a.paint(ansiBold, da.VerificationURI), a.paint(ansiBold, da.UserCode))
	if da.VerificationURIComplete != "" {
		fmt.Fprintf(a.io.err, "Or open %s\n", da.VerificationURIComplete)
	}
	interval := time.Duration(max(da.Interval, 5)) * time.Second
	deadline := a.now().Add(time.Duration(max(da.ExpiresIn, 60)) * time.Second)
	for {
		if a.now().After(deadline) {
			return errors.New("the device code expired before the sign-in finished")
		}
		if err := pollSleep(a.ctx, interval); err != nil {
			return err
		}
		tok, err := a.tokenRequest(a.ctx, endpoint, url.Values{
			"grant_type": {deviceGrant}, "device_code": {da.DeviceCode}, "client_id": {appID},
		})
		var oe *oauthError
		if errors.As(err, &oe) {
			switch oe.code {
			case "authorization_pending":
				continue
			case "slow_down":
				interval += 5 * time.Second
				continue
			}
		}
		if err != nil {
			return fmt.Errorf("sign-in failed: %w", err)
		}
		path, err := a.credentialPath()
		if err != nil {
			return err
		}
		cred := &storedCredential{LogtoEndpoint: endpoint, AppID: appID, Resource: resource, AccessToken: tok.AccessToken,
			RefreshToken: tok.RefreshToken, ExpiresAt: a.now().Add(time.Duration(tok.ExpiresIn) * time.Second)}
		if err := writeCredential(path, cred); err != nil {
			return fmt.Errorf("store the tokens: %w", err)
		}
		fmt.Fprintf(a.io.err, "Signed in. Tokens are stored in %s\n", path)
		if a.getenv(ownsand.EnvAPIKey) != "" {
			a.warnf("OWNSAND_API_KEY is set; commands use it instead of the stored tokens")
		}
		return nil
	}
}

func logoutCommand() command {
	return command{run: func(a *app, _ *flag.FlagSet, args []string) error {
		if len(args) != 0 {
			return usagef("logout takes no arguments")
		}
		path, err := a.credentialPath()
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				fmt.Fprintln(a.io.err, "Not signed in.")
				return nil
			}
			return err
		}
		fmt.Fprintln(a.io.err, "Signed out; the stored tokens are deleted.")
		return nil
	}}
}
