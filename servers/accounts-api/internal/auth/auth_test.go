// SPDX-License-Identifier: FSL-1.1-ALv2

package auth_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/accounts-api/internal/auth"
	"github.com/delinoio/mono/servers/accounts-api/internal/auth/authtest"
	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func newValidator(iss *authtest.Issuer) *auth.LogtoValidator {
	return auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   iss.URL,
		Audience: authtest.Audience,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
}

func reasonOf(t *testing.T, err error) auth.RejectReason {
	t.Helper()
	var invalid *auth.InvalidTokenError
	if !errors.As(err, &invalid) {
		t.Fatalf("error = %v, want *InvalidTokenError", err)
	}
	return invalid.Reason
}

func TestValidateAcceptsUserAccessToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	tok, err := newValidator(iss).Validate(context.Background(), iss.UserToken(t, "user-1"))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if tok.Subject != "user-1" || tok.MachineToMachine() || !tok.HasScope(auth.ScopeBilling) {
		t.Fatalf("token = %+v", tok)
	}
}

func TestValidateMachineToken(t *testing.T) {
	iss := authtest.NewIssuer(t)
	tok, err := newValidator(iss).Validate(context.Background(), iss.MachineToken(t, "app-1", auth.ScopeReportUsage))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if !tok.MachineToMachine() {
		t.Fatal("MachineToMachine() = false")
	}
}

func TestValidateRejects(t *testing.T) {
	iss := authtest.NewIssuer(t)
	other := authtest.NewIssuer(t)
	v := newValidator(iss)
	for name, tc := range map[string]struct {
		raw  string
		want auth.RejectReason
	}{
		"garbage":        {"not-a-jwt", auth.RejectMalformed},
		"id token":       {iss.IDToken(t, "user-1"), auth.RejectTokenType},
		"wrong audience": {iss.Sign(t, authtest.Claims{Subject: "u", Audience: "https://other.example", Scopes: []string{auth.ScopeBilling}}), auth.RejectClaims},
		"expired":        {iss.Sign(t, authtest.Claims{Subject: "u", Expiry: time.Now().Add(-time.Hour)}), auth.RejectClaims},
		"no subject":     {iss.Sign(t, authtest.Claims{Scopes: []string{auth.ScopeBilling}}), auth.RejectClaims},
		"other key":      {other.Sign(t, authtest.Claims{Subject: "u"}), auth.RejectSignature},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := v.Validate(context.Background(), tc.raw)
			if got := reasonOf(t, err); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestValidateRejectsWrongIssuer(t *testing.T) {
	iss := authtest.NewIssuer(t)
	v := auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   "https://elsewhere.example/oidc",
		Audience: authtest.Audience,
		JWKSURL:  iss.JWKSURL,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	_, err := v.Validate(context.Background(), iss.UserToken(t, "u"))
	if got := reasonOf(t, err); got != auth.RejectClaims {
		t.Fatalf("reason = %q, want claims", got)
	}
}

func TestValidateUnreachableKeySet(t *testing.T) {
	v := auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   "http://127.0.0.1:1/oidc",
		Audience: authtest.Audience,
		Client:   &http.Client{Timeout: time.Second},
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	iss := authtest.NewIssuer(t)
	_, err := v.Validate(context.Background(), iss.UserToken(t, "u"))
	if got := reasonOf(t, err); got != auth.RejectUnknownKey {
		t.Fatalf("reason = %q, want unknown_key", got)
	}
}

// stallableJWKS proxies the key set of an issuer. While stalled, it holds
// each request until release and reports the start on started.
type stallableJWKS struct {
	url      string
	requests atomic.Int32
	started  chan struct{}

	mu      sync.Mutex
	release chan struct{}
}

func newStallableJWKS(t *testing.T, iss *authtest.Issuer) *stallableJWKS {
	t.Helper()
	s := &stallableJWKS{started: make(chan struct{}, 8)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.requests.Add(1)
		s.mu.Lock()
		release := s.release
		s.mu.Unlock()
		if release != nil {
			s.started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		resp, err := http.Get(iss.JWKSURL)
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, resp.Body)
	}))
	t.Cleanup(func() {
		s.unstall()
		srv.Close()
	})
	s.url = srv.URL
	return s
}

func (s *stallableJWKS) stall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.release = make(chan struct{})
}

func (s *stallableJWKS) unstall() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.release != nil {
		close(s.release)
		s.release = nil
	}
}

func (s *stallableJWKS) waitStarted(t *testing.T) {
	t.Helper()
	select {
	case <-s.started:
	case <-time.After(5 * time.Second):
		t.Fatal("key set request did not start")
	}
}

// settableClock is a clock that tests advance.
type settableClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *settableClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *settableClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newStalledValidator(iss *authtest.Issuer, jwksURL string, now func() time.Time) *auth.LogtoValidator {
	return auth.NewLogtoValidator(auth.LogtoConfig{
		Issuer:   iss.URL,
		Audience: authtest.Audience,
		JWKSURL:  jwksURL,
		Now:      now,
		Logger:   slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
}

type validation struct {
	token auth.Token
	err   error
}

func startValidate(ctx context.Context, v *auth.LogtoValidator, raw string) <-chan validation {
	ch := make(chan validation, 1)
	go func() {
		tok, err := v.Validate(ctx, raw)
		ch <- validation{token: tok, err: err}
	}()
	return ch
}

func receiveValidation(t *testing.T, ch <-chan validation, what string) validation {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return", what)
		return validation{}
	}
}

func TestValidateCanceledWaiterReturnsWhileRefreshRuns(t *testing.T) {
	iss := authtest.NewIssuer(t)
	jwks := newStallableJWKS(t, iss)
	jwks.stall()
	v := newStalledValidator(iss, jwks.url, nil)

	leader := startValidate(context.Background(), v, iss.UserToken(t, "u"))
	jwks.waitStarted(t)

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiter := startValidate(waiterCtx, v, iss.UserToken(t, "u"))
	cancelWaiter()
	if r := receiveValidation(t, waiter, "canceled waiter"); r.err == nil {
		t.Fatal("canceled waiter validated a token without a key set")
	} else if got := reasonOf(t, r.err); got != auth.RejectUnknownKey {
		t.Fatalf("reason = %q, want unknown_key", got)
	}

	// The refresh that the leader started keeps running.
	select {
	case r := <-leader:
		t.Fatalf("leader returned early: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	jwks.unstall()
	if r := receiveValidation(t, leader, "leader"); r.err != nil || r.token.Subject != "u" {
		t.Fatalf("leader = %+v, want a valid token", r)
	}
	if got := jwks.requests.Load(); got != 1 {
		t.Fatalf("key set requests = %d, want 1", got)
	}
}

func TestValidateCanceledLeaderDoesNotAbortSharedRefresh(t *testing.T) {
	iss := authtest.NewIssuer(t)
	jwks := newStallableJWKS(t, iss)
	jwks.stall()
	v := newStalledValidator(iss, jwks.url, nil)

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := startValidate(leaderCtx, v, iss.UserToken(t, "u"))
	jwks.waitStarted(t)
	waiter := startValidate(context.Background(), v, iss.UserToken(t, "u"))

	cancelLeader()
	if r := receiveValidation(t, leader, "canceled leader"); r.err == nil {
		t.Fatal("canceled leader validated a token without a key set")
	}
	jwks.unstall()
	if r := receiveValidation(t, waiter, "waiter"); r.err != nil || r.token.Subject != "u" {
		t.Fatalf("waiter = %+v, want a valid token from the shared refresh", r)
	}
	if got := jwks.requests.Load(); got != 1 {
		t.Fatalf("key set requests = %d, want 1 (the refresh must be shared)", got)
	}
}

// unknownKeyToken returns a token that is signed with a key that the key set
// does not contain.
func unknownKeyToken(t *testing.T, iss *authtest.Issuer) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	opts := (&jose.SignerOptions{}).WithType("at+jwt").WithHeader(jose.HeaderKey("kid"), "rotated-key")
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(map[string]any{
		"iss": iss.URL, "sub": "u", "aud": authtest.Audience,
		"exp": jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestValidateUsesCachedKeyDuringUnrelatedRefresh(t *testing.T) {
	iss := authtest.NewIssuer(t)
	jwks := newStallableJWKS(t, iss)
	clock := &settableClock{now: time.Now()}
	v := newStalledValidator(iss, jwks.url, clock.Now)

	if _, err := v.Validate(context.Background(), iss.UserToken(t, "u")); err != nil {
		t.Fatal(err)
	}
	if got := jwks.requests.Load(); got != 1 {
		t.Fatalf("key set requests = %d, want 1", got)
	}

	// Past the cooldown, a token with an unknown key ID starts a refresh.
	clock.advance(2 * time.Minute)
	jwks.stall()
	rotated := startValidate(context.Background(), v, unknownKeyToken(t, iss))
	jwks.waitStarted(t)

	// A token with the cached key does not wait for that refresh.
	if r := receiveValidation(t, startValidate(context.Background(), v, iss.UserToken(t, "u")), "cached key request"); r.err != nil {
		t.Fatalf("cached key request error = %v", r.err)
	}

	jwks.unstall()
	if r := receiveValidation(t, rotated, "unknown key request"); r.err == nil {
		t.Fatal("token with an unknown key validated")
	} else if got := reasonOf(t, r.err); got != auth.RejectUnknownKey {
		t.Fatalf("reason = %q, want unknown_key", got)
	}
	if got := jwks.requests.Load(); got != 2 {
		t.Fatalf("key set requests = %d, want 2", got)
	}

	// The cooldown still holds: another unknown key ID is not fetched.
	if _, err := v.Validate(context.Background(), unknownKeyToken(t, iss)); err == nil {
		t.Fatal("token with an unknown key validated")
	}
	if got := jwks.requests.Load(); got != 2 {
		t.Fatalf("key set requests = %d, want 2 within the cooldown", got)
	}
}
