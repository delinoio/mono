// SPDX-License-Identifier: FSL-1.1-ALv2

// Package auth validates Logto access tokens and applies the scope and token
// kind of each Connect service. The owning contracts are
// docs/repository-auth-contract.md (Token validation) and
// docs/servers-accounts-api-foundation.md (Authentication).
//
// The validator follows servers/ownsand-api/internal/auth/logto.go. It differs
// in what it returns: accounts-api answers a valid token without the scope of
// the service, and a machine-to-machine token on a user service, with
// permission_denied rather than unauthenticated, so Validate returns the
// scopes and the token kind and the interceptor decides.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// signatureAlgorithms are the JWS algorithms that the validator accepts.
// Logto signs with EC keys (ES384 by default) or RSA keys.
var signatureAlgorithms = []jose.SignatureAlgorithm{
	jose.ES256, jose.ES384, jose.ES512,
	jose.RS256, jose.RS384, jose.RS512,
	jose.PS256, jose.PS384, jose.PS512,
	jose.EdDSA,
}

// clockLeeway tolerates small clock differences in exp and nbf checks.
const clockLeeway = 30 * time.Second

// jwksRefreshInterval is the cache lifetime of a fetched key set, and
// jwksMinRefresh the shortest interval between two fetch attempts, whether
// the previous attempt succeeded or failed.
const (
	jwksRefreshInterval = 10 * time.Minute
	jwksMinRefresh      = time.Minute
)

// maxJWKSBytes bounds the key set response.
const maxJWKSBytes = 1 << 20

// RejectReason says why a token failed validation. It goes into the
// `auth_rejected` log record; clients get only unauthenticated.
type RejectReason string

const (
	RejectMalformed    RejectReason = "malformed"
	RejectTokenType    RejectReason = "token_type"
	RejectUnknownKey   RejectReason = "unknown_key"
	RejectSignature    RejectReason = "signature"
	RejectClaims       RejectReason = "claims"
	RejectMissingToken RejectReason = "missing_token"
)

// InvalidTokenError is returned for every token that fails validation.
type InvalidTokenError struct {
	Reason RejectReason
}

func (e *InvalidTokenError) Error() string { return "invalid access token: " + string(e.Reason) }

func invalid(reason RejectReason) error { return &InvalidTokenError{Reason: reason} }

// Token is a validated Logto access token.
type Token struct {
	// Subject is the Logto `sub`: the user ID for a user token and the
	// application ID for a machine-to-machine token.
	Subject string
	// ClientID is the `client_id` claim.
	ClientID string
	// Scopes are the space-separated values of the `scope` claim.
	Scopes []string
}

// MachineToMachine reports whether the token comes from the Logto client
// credentials grant: Logto sets `sub` to the application ID, equal to
// `client_id`, in such tokens.
func (t Token) MachineToMachine() bool {
	return t.ClientID != "" && t.Subject == t.ClientID
}

// HasScope reports whether the token carries scope.
func (t Token) HasScope(scope string) bool { return slices.Contains(t.Scopes, scope) }

// LogtoConfig configures the Logto access token validator.
type LogtoConfig struct {
	// Issuer is the expected `iss`, `<tenant endpoint>/oidc`.
	Issuer string
	// Audience is the expected `aud`, the API resource indicator.
	Audience string
	// JWKSURL is the key set URL; empty selects `<Issuer>/jwks`.
	JWKSURL string
	Client  *http.Client
	Now     func() time.Time
	Logger  *slog.Logger
}

// LogtoValidator validates Logto access tokens: signature, issuer, expiry,
// audience and the RFC 9068 token type, which rejects ID tokens.
type LogtoValidator struct {
	cfg LogtoConfig

	// mu guards keys, fetchedAt and attemptedAt. It is held only for memory
	// access, never across the key set request.
	mu        sync.Mutex
	keys      jose.JSONWebKeySet
	fetchedAt time.Time
	// attemptedAt is the start time of the last refresh attempt, successful
	// or not. It gates both refresh paths so that a failing key set endpoint
	// is not fetched again within jwksMinRefresh.
	attemptedAt time.Time

	// refreshGate admits one key set refresh at a time. It is a channel with
	// capacity one and not a mutex so that a request waits for it with its
	// context: a request that is canceled while another refresh runs
	// returns at once.
	refreshGate chan struct{}
}

// NewLogtoValidator returns a validator. It does not fetch keys until the
// first token arrives.
func NewLogtoValidator(cfg LogtoConfig) *LogtoValidator {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: 10 * time.Second}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.JWKSURL == "" {
		cfg.JWKSURL = strings.TrimRight(cfg.Issuer, "/") + "/jwks"
	}
	return &LogtoValidator{cfg: cfg, refreshGate: make(chan struct{}, 1)}
}

type logtoClaims struct {
	jwt.Claims
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
}

// Validate checks a bearer token. Every failure is an *InvalidTokenError.
func (v *LogtoValidator) Validate(ctx context.Context, raw string) (Token, error) {
	tok, err := jwt.ParseSigned(raw, signatureAlgorithms)
	if err != nil || len(tok.Headers) != 1 {
		return Token{}, invalid(RejectMalformed)
	}
	header := tok.Headers[0]
	// Logto access tokens are RFC 9068 JWTs with typ `at+jwt`. ID tokens
	// have another type, and their audience is the client ID; both checks
	// reject them.
	typ, _ := header.ExtraHeaders[jose.HeaderType].(string)
	if t := strings.ToLower(typ); t != "at+jwt" && t != "application/at+jwt" {
		return Token{}, invalid(RejectTokenType)
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return Token{}, invalid(RejectUnknownKey)
	}
	var claims logtoClaims
	if err := tok.Claims(key, &claims); err != nil {
		return Token{}, invalid(RejectSignature)
	}
	if claims.Expiry == nil || claims.Subject == "" {
		return Token{}, invalid(RejectClaims)
	}
	expected := jwt.Expected{Issuer: v.cfg.Issuer, AnyAudience: jwt.Audience{v.cfg.Audience}, Time: v.cfg.Now()}
	if err := claims.ValidateWithLeeway(expected, clockLeeway); err != nil {
		return Token{}, invalid(RejectClaims)
	}
	return Token{Subject: claims.Subject, ClientID: claims.ClientID, Scopes: strings.Fields(claims.Scope)}, nil
}

// key returns the signing key with the key ID. A cached key that needs no
// refresh is returned without waiting for a refresh that another request runs.
// Requests that need a refresh share it: one runs it and the others wait for
// it, each ending its wait when its own context ends.
func (v *LogtoValidator) key(ctx context.Context, kid string) (*jose.JSONWebKey, error) {
	k, due := v.lookup(kid)
	if k != nil && !due {
		return k, nil
	}
	// A request that finds no key waits for a refresh that is already
	// running before it decides: that refresh can load the key, and during
	// the cooldown it is the reason why no new refresh starts.
	select {
	case v.refreshGate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	// Another request can have refreshed, or armed the cooldown, while this
	// one waited.
	if k, due = v.lookup(kid); !due {
		<-v.refreshGate
		return keyOrUnknown(k)
	}
	err := v.refresh(ctx)
	if err != nil && (ctx.Err() != nil || !v.hasKeys()) {
		return nil, err
	}
	// The refresh armed the cooldown, so an unknown key ID is not fetched
	// again for jwksMinRefresh, even when the refresh failed.
	k, _ = v.lookup(kid)
	return keyOrUnknown(k)
}

func keyOrUnknown(k *jose.JSONWebKey) (*jose.JSONWebKey, error) {
	if k == nil {
		return nil, errors.New("unknown key id")
	}
	return k, nil
}

// lookup returns the cached key with the key ID and whether a refresh is due.
// A refresh is due outside the cooldown when the key set is missing or older
// than jwksRefreshInterval, or when the key ID is unknown. An unknown key ID
// refreshes the key set at most once per jwksMinRefresh, counted from the last
// attempt even when it failed.
func (v *LogtoValidator) lookup(kid string) (key *jose.JSONWebKey, due bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	now := v.cfg.Now()
	cooling := !v.attemptedAt.IsZero() && now.Sub(v.attemptedAt) < jwksMinRefresh
	stale := v.fetchedAt.IsZero() || now.Sub(v.fetchedAt) > jwksRefreshInterval
	key = v.findLocked(kid)
	return key, !cooling && (stale || key == nil)
}

func (v *LogtoValidator) hasKeys() bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	return !v.fetchedAt.IsZero()
}

func (v *LogtoValidator) findLocked(kid string) *jose.JSONWebKey {
	for i := range v.keys.Keys {
		k := &v.keys.Keys[i]
		if k.KeyID == kid && k.IsPublic() && (k.Use == "" || k.Use == "sig") {
			return k
		}
	}
	return nil
}

// refresh loads the key set. The caller holds refreshGate; refresh releases
// it when the load ends. The load runs in its own goroutine, so a caller that
// is canceled while it waits returns at once and the load still completes for
// the other callers.
func (v *LogtoValidator) refresh(ctx context.Context) error {
	// A caller that is already gone must not arm the process-wide cooldown
	// for later live callers.
	if err := ctx.Err(); err != nil {
		<-v.refreshGate
		return err
	}
	v.mu.Lock()
	v.attemptedAt = v.cfg.Now()
	v.mu.Unlock()
	done := make(chan error, 1)
	go func() {
		defer func() { <-v.refreshGate }()
		done <- v.load(ctx)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// load fetches the key set and replaces the cached one. It holds no lock
// during the request.
func (v *LogtoValidator) load(ctx context.Context) error {
	body, err := v.fetch(ctx)
	if err != nil {
		v.cfg.Logger.Warn("logto_jwks_fetch_failed", slog.Any("error", err))
		return err
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		v.cfg.Logger.Warn("logto_jwks_parse_failed", slog.Any("error", err))
		return err
	}
	v.mu.Lock()
	v.keys = set
	v.fetchedAt = v.cfg.Now()
	v.mu.Unlock()
	v.cfg.Logger.Info("logto_jwks_loaded", slog.Int("keys", len(set.Keys)))
	return nil
}

func (v *LogtoValidator) fetch(ctx context.Context) ([]byte, error) {
	// The fetch serves every caller and arms a shared cooldown, so one
	// caller's cancellation must not abort it; only the timeout bounds it.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.cfg.JWKSURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := v.cfg.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxJWKSBytes))
}
