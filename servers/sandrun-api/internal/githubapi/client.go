// SPDX-License-Identifier: FSL-1.1-ALv2

// Package githubapi is the GitHub REST client of sandrun-api for the SandRun
// GitHub App, on net/http without a third-party client: the app JWT
// (RS256), installation access tokens (cached until shortly before they
// expire), the organization self-hosted runner endpoints (runner groups, JIT
// configurations, runner lookup and removal), the OAuth code exchange with
// GET /user/installations, and the app webhook deliveries with redelivery.
// It also verifies webhook signatures.
//
// Every GitHub API error is logged with its operation and HTTP status. Tokens,
// JIT configurations, OAuth codes and response bodies are never logged. The
// owning contract is the GitHub App integration section of
// docs/servers-sandrun-api-foundation.md.
package githubapi

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
)

// APIVersion is the X-GitHub-Api-Version that SandRun sends.
const APIVersion = "2026-03-10"

// maxResponseBytes bounds a GitHub response body that SandRun reads.
const maxResponseBytes = 4 << 20

// tokenRefreshMargin renews a cached installation token this long before it
// expires.
const tokenRefreshMargin = 5 * time.Minute

// Config configures a Client.
type Config struct {
	AppID        int64
	ClientID     string
	ClientSecret string
	PrivateKey   *rsa.PrivateKey
	Slug         string
	// APIOrigin is the REST API origin (https://api.github.com).
	APIOrigin string
	// WebOrigin serves the installation page and the OAuth code exchange
	// (https://github.com).
	WebOrigin string
}

// Client calls the GitHub REST API as the SandRun GitHub App.
type Client struct {
	cfg    Config
	http   *http.Client
	logger *slog.Logger
	now    func() time.Time

	mu     sync.Mutex
	tokens map[int64]cachedToken
}

type cachedToken struct {
	token  string
	expiry time.Time
}

// New returns a client. httpClient nil selects a client with a 30-second
// timeout; now nil selects time.Now.
func New(cfg Config, httpClient *http.Client, logger *slog.Logger, now func() time.Time) *Client {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if now == nil {
		now = time.Now
	}
	cfg.APIOrigin = strings.TrimSuffix(cfg.APIOrigin, "/")
	cfg.WebOrigin = strings.TrimSuffix(cfg.WebOrigin, "/")
	return &Client{cfg: cfg, http: httpClient, logger: logger, now: now, tokens: map[int64]cachedToken{}}
}

// AppID returns the numeric app ID.
func (c *Client) AppID() int64 { return c.cfg.AppID }

// Error is a GitHub API answer with an unexpected status. It holds no
// response body, because a body can echo request data.
type Error struct {
	Op     string
	Status int
}

func (e *Error) Error() string { return fmt.Sprintf("github %s: HTTP %d", e.Op, e.Status) }

// StatusOf returns the HTTP status of a GitHub API error, or 0.
func StatusOf(err error) int {
	var e *Error
	if errors.As(err, &e) {
		return e.Status
	}
	return 0
}

// ErrRunnerRefused is the answer of DeleteRunner when GitHub refuses the
// removal with 422. GitHub documents only 204 and 422 for this endpoint and
// does not document the answer for a runner that runs a job, so SandRun treats
// every 422 as "the runner took a job" without reading the message.
var ErrRunnerRefused = errors.New("github refused the runner removal")

// IsNotFound reports a 404 answer.
func IsNotFound(err error) bool { return StatusOf(err) == http.StatusNotFound }

// AppJWT returns a new app JWT: RS256, iat 60 seconds in the past for clock
// drift, exp 9 minutes ahead (GitHub accepts at most 10) and iss the client
// ID, as GitHub recommends.
func (c *Client) AppJWT() (string, error) {
	now := c.now().Unix()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 9*60, "iss": c.cfg.ClientID})
	if err != nil {
		return "", err
	}
	signingInput := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.cfg.PrivateKey, crypto.SHA256, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign the app JWT: %w", err)
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// request is one GitHub call.
type request struct {
	op     string
	method string
	url    string
	// auth is the bearer token; empty sends none.
	auth string
	body any
	// form sends an application/x-www-form-urlencoded body instead.
	form url.Values
	// ok lists the statuses that succeed; out receives their JSON body.
	ok  []int
	out any
	// installationID identifies the cache entry to check on a 401.
	installationID int64
}

// do sends a request and decodes the answer. It returns the response
// headers of a successful answer.
func (c *Client) do(ctx context.Context, r request) (http.Header, error) {
	var body io.Reader
	contentType := ""
	switch {
	case r.form != nil:
		body = strings.NewReader(r.form.Encode())
		contentType = "application/x-www-form-urlencoded"
	case r.body != nil:
		b, err := json.Marshal(r.body)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
		contentType = "application/json"
	}
	req, err := http.NewRequestWithContext(ctx, r.method, r.url, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", "sandrun-api")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if r.form != nil {
		// The OAuth endpoint answers form-encoded unless JSON is accepted.
		req.Header.Set("Accept", "application/json")
	}
	if r.auth != "" {
		req.Header.Set("Authorization", "Bearer "+r.auth)
	}
	if err := api.CheckTeardown(ctx); err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() == nil {
			c.logger.WarnContext(ctx, "github_api_error", slog.String("op", r.op), slog.Int("status", 0), slog.String("error", "request failed"))
		}
		// The error of net/http names the URL, which holds no secret.
		return nil, fmt.Errorf("github %s: %w", r.op, err)
	}
	defer resp.Body.Close()
	for _, s := range r.ok {
		if resp.StatusCode != s {
			continue
		}
		if r.out != nil {
			if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBytes)).Decode(r.out); err != nil {
				c.logger.WarnContext(ctx, "github_api_error", slog.String("op", r.op), slog.Int("status", resp.StatusCode), slog.String("error", "invalid response body"))
				return nil, fmt.Errorf("github %s: invalid response body", r.op)
			}
		}
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
		return resp.Header, nil
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode == http.StatusUnauthorized && r.installationID != 0 {
		c.dropToken(r.installationID, r.auth)
	}
	c.logger.WarnContext(ctx, "github_api_error", slog.String("op", r.op), slog.Int("status", resp.StatusCode))
	return nil, &Error{Op: r.op, Status: resp.StatusCode}
}

func (c *Client) api(path string) string { return c.cfg.APIOrigin + path }

// InstallationToken returns an installation access token, from the cache
// while it is valid for more than tokenRefreshMargin.
func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	now := c.now()
	c.mu.Lock()
	t, ok := c.tokens[installationID]
	c.mu.Unlock()
	if ok && now.Add(tokenRefreshMargin).Before(t.expiry) {
		return t.token, nil
	}
	jwt, err := c.AppJWT()
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	if _, err := c.do(ctx, request{op: "create_installation_token", method: http.MethodPost,
		url: c.api("/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"), auth: jwt,
		ok: []int{http.StatusCreated}, out: &out}); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", errors.New("github create_installation_token: empty token")
	}
	if out.ExpiresAt.IsZero() {
		// GitHub documents a 1-hour lifetime.
		out.ExpiresAt = now.Add(time.Hour)
	}
	c.mu.Lock()
	c.tokens[installationID] = cachedToken{token: out.Token, expiry: out.ExpiresAt}
	c.mu.Unlock()
	return out.Token, nil
}

func (c *Client) dropToken(installationID int64, rejectedToken string) {
	c.mu.Lock()
	if cached, ok := c.tokens[installationID]; ok && cached.token == rejectedToken {
		delete(c.tokens, installationID)
	}
	c.mu.Unlock()
}

// installationRequest sends a request with the installation token.
func (c *Client) installationRequest(ctx context.Context, installationID int64, r request) (http.Header, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	r.auth = token
	r.installationID = installationID
	return c.do(ctx, r)
}

// RunnerGroup is a self-hosted runner group of an organization.
type RunnerGroup struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Visibility string `json:"visibility"`
	Default    bool   `json:"default"`
}

func orgPath(org string) string { return "/orgs/" + url.PathEscape(org) + "/actions" }

// RunnerGroups lists the runner groups of the installation's organization.
func (c *Client) RunnerGroups(ctx context.Context, installationID int64, org string) ([]RunnerGroup, error) {
	var all []RunnerGroup
	for page := 1; page <= 50; page++ {
		var out struct {
			TotalCount   int           `json:"total_count"`
			RunnerGroups []RunnerGroup `json:"runner_groups"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if _, err := c.installationRequest(ctx, installationID, request{op: "list_runner_groups", method: http.MethodGet,
			url: c.api(orgPath(org) + "/runner-groups?" + q.Encode()), ok: []int{http.StatusOK}, out: &out}); err != nil {
			return nil, err
		}
		all = append(all, out.RunnerGroups...)
		if len(out.RunnerGroups) < 100 || len(all) >= out.TotalCount {
			break
		}
	}
	return all, nil
}

// Runner is a self-hosted runner of an organization.
type Runner struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	RunnerGroupID int64  `json:"runner_group_id"`
	Status        string `json:"status"`
	Busy          bool   `json:"busy"`
	Ephemeral     bool   `json:"ephemeral"`
}

// JITConfig is the answer of GenerateJITConfig. EncodedJITConfig is a
// secret: pass it only to the runner and never store or log it.
type JITConfig struct {
	Runner           Runner `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// ErrRunnerNameTaken is the 409 answer of GenerateJITConfig. GitHub does
// not document the cause of 409; a runner with the same name is the likely
// one.
var ErrRunnerNameTaken = errors.New("github refused the JIT configuration with 409 (a runner with the name may exist)")

// GenerateJITConfig creates a JIT runner configuration with
// POST /orgs/{org}/actions/runners/generate-jitconfig.
func (c *Client) GenerateJITConfig(ctx context.Context, installationID int64, org, name string, runnerGroupID int64, labels []string) (*JITConfig, error) {
	var out JITConfig
	_, err := c.installationRequest(ctx, installationID, request{op: "generate_jitconfig", method: http.MethodPost,
		url:  c.api(orgPath(org) + "/runners/generate-jitconfig"),
		body: map[string]any{"name": name, "runner_group_id": runnerGroupID, "labels": labels},
		ok:   []int{http.StatusCreated}, out: &out})
	if StatusOf(err) == http.StatusConflict {
		return nil, ErrRunnerNameTaken
	}
	if err != nil {
		return nil, err
	}
	if out.EncodedJITConfig == "" || out.Runner.ID == 0 {
		return nil, errors.New("github generate_jitconfig: incomplete response")
	}
	return &out, nil
}

// FindRunner returns the runner of the organization with the exact name,
// or nil.
func (c *Client) FindRunner(ctx context.Context, installationID int64, org, name string) (*Runner, error) {
	var out struct {
		Runners []Runner `json:"runners"`
	}
	q := url.Values{"name": {name}, "per_page": {"100"}}
	if _, err := c.installationRequest(ctx, installationID, request{op: "list_runners", method: http.MethodGet,
		url: c.api(orgPath(org) + "/runners?" + q.Encode()), ok: []int{http.StatusOK}, out: &out}); err != nil {
		return nil, err
	}
	for i := range out.Runners {
		if out.Runners[i].Name == name {
			return &out.Runners[i], nil
		}
	}
	return nil, nil
}

// DeleteRunner removes a runner registration. A runner that is already gone
// (404) counts as removed. A 422 answer returns ErrRunnerRefused.
func (c *Client) DeleteRunner(ctx context.Context, installationID int64, org string, runnerID int64) error {
	_, err := c.installationRequest(ctx, installationID, request{op: "delete_runner", method: http.MethodDelete,
		url: c.api(orgPath(org) + "/runners/" + strconv.FormatInt(runnerID, 10)), ok: []int{http.StatusNoContent}})
	switch StatusOf(err) {
	case http.StatusNotFound:
		return nil
	case http.StatusUnprocessableEntity:
		return ErrRunnerRefused
	}
	return err
}

// ErrBadCode is the answer of ExchangeCode for a code that GitHub does not
// accept (expired, used or of another app).
var ErrBadCode = errors.New("github rejected the OAuth code")

// ExchangeCode exchanges an OAuth code of the installation callback for a
// user access token. The caller discards the token after its check.
func (c *Client) ExchangeCode(ctx context.Context, code string) (string, error) {
	var out struct {
		AccessToken string `json:"access_token"`
		Error       string `json:"error"`
	}
	form := url.Values{"client_id": {c.cfg.ClientID}, "client_secret": {c.cfg.ClientSecret}, "code": {code}}
	if _, err := c.do(ctx, request{op: "exchange_oauth_code", method: http.MethodPost, url: c.cfg.WebOrigin + "/login/oauth/access_token",
		form: form, ok: []int{http.StatusOK}, out: &out}); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		// GitHub answers an invalid code with 200 and an `error` field (for
		// example bad_verification_code); the web flow error names are not
		// documented, so any answer without a token counts as a bad code.
		c.logger.WarnContext(ctx, "github_api_error", slog.String("op", "exchange_oauth_code"), slog.Int("status", http.StatusOK), slog.String("oauth_error", safeCode(out.Error)))
		return "", ErrBadCode
	}
	return out.AccessToken, nil
}

// safeCode keeps a short identifier-like error code for the log.
func safeCode(s string) string {
	if len(s) > 64 {
		return "other"
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; !(c == '_' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9') {
			return "other"
		}
	}
	return s
}

// Account is the account of an installation.
type Account struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
	Type  string `json:"type"`
}

// Installation is an installation as GET /user/installations and the
// installation webhook report it.
type Installation struct {
	ID          int64   `json:"id"`
	AppID       int64   `json:"app_id"`
	Account     Account `json:"account"`
	TargetType  string  `json:"target_type"`
	SuspendedAt *string `json:"suspended_at"`
}

// IsOrganization reports an installation on an organization account.
// GitHub's example payload pairs account.type "User" with target_type
// "Organization", so SandRun requires both fields to say Organization.
func (i Installation) IsOrganization() bool {
	return i.TargetType == "Organization" && i.Account.Type == "Organization"
}

// UserInstallations lists the installations of the app that the user
// access token can access.
func (c *Client) UserInstallations(ctx context.Context, userToken string) ([]Installation, error) {
	var all []Installation
	for page := 1; page <= 50; page++ {
		var out struct {
			TotalCount    int            `json:"total_count"`
			Installations []Installation `json:"installations"`
		}
		q := url.Values{"per_page": {"100"}, "page": {strconv.Itoa(page)}}
		if _, err := c.do(ctx, request{op: "list_user_installations", method: http.MethodGet, url: c.api("/user/installations?" + q.Encode()),
			auth: userToken, ok: []int{http.StatusOK}, out: &out}); err != nil {
			return nil, err
		}
		all = append(all, out.Installations...)
		if len(out.Installations) < 100 || len(all) >= out.TotalCount {
			break
		}
	}
	return all, nil
}

// InstallURL returns the installation page of the app with the state.
func (c *Client) InstallURL(state string) string {
	return c.cfg.WebOrigin + "/apps/" + url.PathEscape(c.cfg.Slug) + "/installations/new?" + url.Values{"state": {state}}.Encode()
}

// Delivery is one delivery of the app webhook.
type Delivery struct {
	ID          int64     `json:"id"`
	GUID        string    `json:"guid"`
	DeliveredAt time.Time `json:"delivered_at"`
	Redelivery  bool      `json:"redelivery"`
	Status      string    `json:"status"`
	StatusCode  int       `json:"status_code"`
	Event       string    `json:"event"`
}

// Succeeded reports a delivery that the receiver answered with 2xx (status
// "OK" in GitHub's list).
func (d Delivery) Succeeded() bool {
	return d.Status == "OK" || (d.StatusCode >= 200 && d.StatusCode < 300)
}

// FailedDeliveries lists the failed deliveries of the app webhook that
// GitHub delivered at or after since, newest first, with
// GET /app/hook/deliveries?status=failure and the Link cursor. The list
// stops at the end of the listing or on a page with an entry older than since.
// GitHub lists the newest first, although the docs do not state the order
// explicitly.
func (c *Client) FailedDeliveries(ctx context.Context, since time.Time) ([]Delivery, error) {
	jwt, err := c.AppJWT()
	if err != nil {
		return nil, err
	}
	next := c.api("/app/hook/deliveries?" + url.Values{"per_page": {"100"}, "status": {"failure"}}.Encode())
	var out []Delivery
	visited := make(map[string]struct{})
	for next != "" {
		if _, seen := visited[next]; seen {
			return nil, errors.New("github list_hook_deliveries: pagination cycle")
		}
		visited[next] = struct{}{}
		var items []Delivery
		h, err := c.do(ctx, request{op: "list_hook_deliveries", method: http.MethodGet, url: next, auth: jwt, ok: []int{http.StatusOK}, out: &items})
		if err != nil {
			return nil, err
		}
		done := false
		for _, d := range items {
			if d.DeliveredAt.Before(since) {
				done = true
				continue
			}
			out = append(out, d)
		}
		if done {
			break
		}
		next = nextLink(h.Get("Link"))
		if next != "" && !strings.HasPrefix(next, c.cfg.APIOrigin+"/") {
			// Follow only links of the configured API origin.
			next = ""
		}
	}
	return out, nil
}

// nextLink returns the rel="next" URL of a Link header.
func nextLink(header string) string {
	for _, part := range strings.Split(header, ",") {
		part = strings.TrimSpace(part)
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		start, end := strings.Index(part, "<"), strings.Index(part, ">")
		if start >= 0 && end > start {
			return part[start+1 : end]
		}
	}
	return ""
}

// Redeliver asks GitHub to deliver a delivery again with
// POST /app/hook/deliveries/{id}/attempts. GitHub redelivers only
// deliveries of the past 3 days.
func (c *Client) Redeliver(ctx context.Context, deliveryID int64) error {
	jwt, err := c.AppJWT()
	if err != nil {
		return err
	}
	_, err = c.do(ctx, request{op: "redeliver_hook_delivery", method: http.MethodPost,
		url: c.api("/app/hook/deliveries/" + strconv.FormatInt(deliveryID, 10) + "/attempts"), auth: jwt, ok: []int{http.StatusAccepted}})
	return err
}

// SignaturePrefix is the prefix of X-Hub-Signature-256.
const SignaturePrefix = "sha256="

// Sign returns the X-Hub-Signature-256 value of body.
func Sign(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return SignaturePrefix + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks X-Hub-Signature-256 (sha256= and the lowercase hex
// HMAC-SHA256 of the raw body) in constant time.
func VerifySignature(secret, body []byte, header string) bool {
	got, ok := strings.CutPrefix(header, SignaturePrefix)
	if !ok || len(got) != sha256.Size*2 {
		return false
	}
	want := Sign(secret, body)[len(SignaturePrefix):]
	return hmac.Equal([]byte(strings.ToLower(got)), []byte(want))
}
