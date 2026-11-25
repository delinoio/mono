// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// RequiredScope is the scope that every tide RPC requires in the
// Logto access token. Roles inside an organization come from tide's own
// membership table, not from token scopes. The Logto API resource
// `https://tide.api.delino.io` defines it (recorded in
// docs/servers-tide-api-foundation.md, Authentication).
const RequiredScope = "tide:use"

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
	jwksFetchTimeout    = 10 * time.Second
)

// ErrInvalidToken is returned for every token that fails validation. The
// caller turns it into unauthenticated without the detail.
var ErrInvalidToken = errors.New("invalid access token")

var errUnknownKeyID = errors.New("unknown key id")

// LogtoConfig configures the Logto access token validator.
type LogtoConfig struct {
	Issuer   string
	Audience string
	// JWKSURL is the key set URL. JWKSFile, when set, is a local key set
	// file used instead (end-to-end test issuers).
	JWKSURL  string
	JWKSFile string
	Client   *http.Client
	Now      func() time.Time
	Logger   *slog.Logger
}

// LogtoValidator validates Logto access tokens according to
// docs/repository-auth-contract.md (signature, issuer, expiry, audience and
// scope) and rejects ID tokens and machine-to-machine tokens.
type LogtoValidator struct {
	cfg LogtoConfig

	// mu guards cache and refresh state. The fetch and file read run without it.
	mu        sync.Mutex
	keys      jose.JSONWebKeySet
	fetchedAt time.Time
	// attemptedAt starts the cooldown for both successful and failed fetches.
	attemptedAt time.Time
	inflight    *jwksFetch

	// fetchTimeout bounds one HTTP fetch. Tests can shorten it.
	fetchTimeout time.Duration
}

// NewLogtoValidator returns a validator. It does not fetch keys until the
// first token arrives.
func NewLogtoValidator(cfg LogtoConfig) *LogtoValidator {
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: jwksFetchTimeout}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &LogtoValidator{cfg: cfg, fetchTimeout: jwksFetchTimeout}
}

type logtoClaims struct {
	jwt.Claims
	Scope    string `json:"scope"`
	ClientID string `json:"client_id"`
}

// Validate checks a bearer token and returns the Logto user ID (`sub`).
func (v *LogtoValidator) Validate(ctx context.Context, raw string) (string, error) {
	tok, err := jwt.ParseSigned(raw, signatureAlgorithms)
	if err != nil || len(tok.Headers) != 1 {
		return "", ErrInvalidToken
	}
	header := tok.Headers[0]
	// Logto access tokens are RFC 9068 JWTs with typ `at+jwt`. ID tokens
	// have another type, and their audience is the client ID; both checks
	// reject them.
	typ, _ := header.ExtraHeaders[jose.HeaderType].(string)
	if t := strings.ToLower(typ); t != "at+jwt" && t != "application/at+jwt" {
		return "", ErrInvalidToken
	}
	key, err := v.key(ctx, header.KeyID)
	if err != nil {
		return "", ErrInvalidToken
	}
	var claims logtoClaims
	if err := tok.Claims(key, &claims); err != nil {
		return "", ErrInvalidToken
	}
	if claims.Expiry == nil {
		return "", ErrInvalidToken
	}
	expected := jwt.Expected{Issuer: v.cfg.Issuer, AnyAudience: jwt.Audience{v.cfg.Audience}, Time: v.cfg.Now()}
	if err := claims.ValidateWithLeeway(expected, clockLeeway); err != nil {
		return "", ErrInvalidToken
	}
	if claims.Subject == "" {
		return "", ErrInvalidToken
	}
	// Logto machine-to-machine (client credentials) tokens carry the
	// application ID as `sub`, equal to `client_id`. Only end-user tokens
	// become a user principal.
	if claims.ClientID != "" && claims.Subject == claims.ClientID {
		return "", ErrInvalidToken
	}
	if !slices.Contains(strings.Fields(claims.Scope), RequiredScope) {
		return "", ErrInvalidToken
	}
	return claims.Subject, nil
}

// key returns cached keys immediately. A caller that needs an uncached key
// waits for the shared fetch or for its own context to end.
func (v *LogtoValidator) key(ctx context.Context, kid string) (*jose.JSONWebKey, error) {
	v.mu.Lock()
	if err := ctx.Err(); err != nil {
		v.mu.Unlock()
		return nil, err
	}

	now := v.cfg.Now()
	k := v.findLocked(kid)
	cooling := !v.attemptedAt.IsZero() && now.Sub(v.attemptedAt) < jwksMinRefresh
	stale := v.fetchedAt.IsZero() || now.Sub(v.fetchedAt) > jwksRefreshInterval
	call := v.inflight
	if call == nil && !cooling && (k == nil || stale) {
		call = v.startFetchLocked(context.WithoutCancel(ctx), now)
	}
	v.mu.Unlock()

	if k != nil {
		return k, nil
	}
	if call == nil {
		return nil, errUnknownKeyID
	}

	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-call.done:
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if call.err != nil {
		return nil, call.err
	}

	v.mu.Lock()
	k = v.findLocked(kid)
	v.mu.Unlock()
	if k == nil {
		return nil, errUnknownKeyID
	}
	return k, nil
}

// jwksFetch is one shared load. The fetch sets err before closing done.
type jwksFetch struct {
	done chan struct{}
	err  error
}

// startFetchLocked records the attempt while v.mu is held. The goroutine
// loads the key set outside the lock and publishes the result when it finishes.
func (v *LogtoValidator) startFetchLocked(ctx context.Context, now time.Time) *jwksFetch {
	call := &jwksFetch{done: make(chan struct{})}
	v.inflight = call
	v.attemptedAt = now
	go func() {
		set, err := v.load(ctx)
		v.mu.Lock()
		if err == nil {
			v.keys = set
			v.fetchedAt = v.cfg.Now()
		}
		call.err = err
		v.inflight = nil
		close(call.done)
		v.mu.Unlock()
	}()
	return call
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

func (v *LogtoValidator) load(ctx context.Context) (jose.JSONWebKeySet, error) {
	var set jose.JSONWebKeySet
	var body []byte
	var err error
	if v.cfg.JWKSFile != "" {
		body, err = os.ReadFile(v.cfg.JWKSFile)
	} else {
		body, err = v.fetch(ctx)
	}
	if err != nil {
		v.cfg.Logger.Warn("logto_jwks_fetch_failed", slog.Any("error", err))
		return set, err
	}
	if err := json.Unmarshal(body, &set); err != nil {
		v.cfg.Logger.Warn("logto_jwks_parse_failed", slog.Any("error", err))
		return jose.JSONWebKeySet{}, err
	}
	v.cfg.Logger.Info("logto_jwks_loaded", slog.Int("keys", len(set.Keys)))
	return set, nil
}

func (v *LogtoValidator) fetch(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, v.fetchTimeout)
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
	return io.ReadAll(io.LimitReader(resp.Body, 1<<20))
}
