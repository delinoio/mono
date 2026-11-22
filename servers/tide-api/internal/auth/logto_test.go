// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

type tokenFactory struct {
	key    *ecdsa.PrivateKey
	kid    string
	issuer string
}

func (f tokenFactory) sign(t *testing.T, typ string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithHeader("kid", f.kid)
	if typ != "" {
		opts = opts.WithType(jose.ContentType(typ))
	}
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES384, Key: f.key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func (f tokenFactory) jwks(t *testing.T) []byte {
	t.Helper()
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: string(jose.ES384), Use: "sig"}}}
	b, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLogtoValidator(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	f := tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"}
	var fetches atomic.Int32
	jwks := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		_, _ = w.Write(f.jwks(t))
	}))
	defer jwks.Close()
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: f.issuer, Audience: "https://tide.api.delino.io", JWKSURL: jwks.URL, Now: func() time.Time { return now }})
	base := func() map[string]any {
		return map[string]any{
			"iss": f.issuer, "aud": "https://tide.api.delino.io", "sub": "user-1", "client_id": "app-1",
			"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "scope": "openid " + RequiredScope,
		}
	}
	ctx := context.Background()
	sub, err := v.Validate(ctx, f.sign(t, "at+jwt", base()))
	if err != nil || sub != "user-1" {
		t.Fatalf("valid token: %q %v", sub, err)
	}
	cases := map[string]func(c map[string]any) (string, map[string]any){
		"id token type":  func(c map[string]any) (string, map[string]any) { return "JWT", c },
		"wrong audience": func(c map[string]any) (string, map[string]any) { c["aud"] = "app-1"; return "at+jwt", c },
		"wrong issuer":   func(c map[string]any) (string, map[string]any) { c["iss"] = "https://evil/oidc"; return "at+jwt", c },
		"expired": func(c map[string]any) (string, map[string]any) {
			c["exp"] = now.Add(-time.Hour).Unix()
			return "at+jwt", c
		},
		"no expiry":          func(c map[string]any) (string, map[string]any) { delete(c, "exp"); return "at+jwt", c },
		"missing scope":      func(c map[string]any) (string, map[string]any) { c["scope"] = "openid"; return "at+jwt", c },
		"machine to machine": func(c map[string]any) (string, map[string]any) { c["sub"] = "app-1"; return "at+jwt", c },
		"empty subject":      func(c map[string]any) (string, map[string]any) { c["sub"] = ""; return "at+jwt", c },
		"not yet valid": func(c map[string]any) (string, map[string]any) {
			c["nbf"] = now.Add(time.Hour).Unix()
			return "at+jwt", c
		},
		"scope as substring":  func(c map[string]any) (string, map[string]any) { c["scope"] = RequiredScope + "x"; return "at+jwt", c },
		"application at+jwt ": func(c map[string]any) (string, map[string]any) { return "application/at+jwt", c },
	}
	for name, mutate := range cases {
		typ, claims := mutate(base())
		_, err := v.Validate(ctx, f.sign(t, typ, claims))
		if name == "application at+jwt " {
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	// A token signed by another key with the same kid is rejected.
	otherKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	forged := tokenFactory{key: otherKey, kid: "k1", issuer: f.issuer}
	if _, err := v.Validate(ctx, forged.sign(t, "at+jwt", base())); err == nil {
		t.Fatal("forged token accepted")
	}
	// Garbage and an unsigned token.
	if _, err := v.Validate(ctx, "a.b.c"); err == nil {
		t.Fatal("garbage accepted")
	}
	if fetches.Load() != 1 {
		t.Fatalf("jwks fetched %d times", fetches.Load())
	}
}

func TestLogtoValidatorJWKSFile(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	f := tokenFactory{key: key, kid: "test", issuer: "http://127.0.0.1:1/oidc"}
	path := filepath.Join(t.TempDir(), "jwks.json")
	if err := os.WriteFile(path, f.jwks(t), 0o600); err != nil {
		t.Fatal(err)
	}
	v := NewLogtoValidator(LogtoConfig{Issuer: f.issuer, Audience: "https://tide.api.delino.io", JWKSFile: path})
	token := f.sign(t, "at+jwt", map[string]any{
		"iss": f.issuer, "aud": []string{"https://tide.api.delino.io"}, "sub": "e2e-user",
		"exp": time.Now().Add(time.Hour).Unix(), "scope": RequiredScope,
	})
	if sub, err := v.Validate(context.Background(), token); err != nil || sub != "e2e-user" {
		t.Fatalf("file jwks: %q %v", sub, err)
	}
}

// jwksOutage serves a key set that tests switch between healthy, failing
// with 503 and malformed.
type jwksOutage struct {
	f       tokenFactory
	fetches atomic.Int32
	mode    atomic.Int32 // 0 valid, 1 status 503, 2 malformed JSON
	extra   atomic.Pointer[tokenFactory]
	// block, when non-nil, holds every healthy response until it is closed.
	block   chan struct{}
	started chan struct{}
}

func (o *jwksOutage) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	o.fetches.Add(1)
	switch o.mode.Load() {
	case 1:
		w.WriteHeader(http.StatusServiceUnavailable)
	case 2:
		_, _ = w.Write([]byte("{not json"))
	default:
		if o.block != nil {
			select {
			case o.started <- struct{}{}:
			default:
			}
			<-o.block
		}
		set := jose.JSONWebKeySet{}
		for _, f := range []*tokenFactory{&o.f, o.extra.Load()} {
			if f != nil {
				set.Keys = append(set.Keys, jose.JSONWebKey{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: string(jose.ES384), Use: "sig"})
			}
		}
		b, _ := json.Marshal(set)
		_, _ = w.Write(b)
	}
}

func outageClaims(f tokenFactory, now time.Time) map[string]any {
	return map[string]any{
		"iss": f.issuer, "aud": "https://tide.api.delino.io", "sub": "user-1",
		"exp": now.Add(24 * time.Hour).Unix(), "scope": RequiredScope,
	}
}

func TestLogtoValidatorBacksOffFailedRefresh(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	o := &jwksOutage{f: tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"}}
	srv := httptest.NewServer(o)
	defer srv.Close()
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://tide.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
	ctx := context.Background()
	known := func() string { return o.f.sign(t, "at+jwt", outageClaims(o.f, now)) }
	otherKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	other := tokenFactory{key: otherKey, kid: "k2", issuer: o.f.issuer}
	unknown := func() string { return other.sign(t, "at+jwt", outageClaims(other, now)) }

	if _, err := v.Validate(ctx, known()); err != nil {
		t.Fatal(err)
	}
	// The cache goes stale and the endpoint fails: exactly one extra fetch
	// serves known keys, an unknown key and repeats within the cooldown.
	now = now.Add(11 * time.Minute)
	o.mode.Store(1)
	for i := 0; i < 2; i++ {
		if sub, err := v.Validate(ctx, known()); err != nil || sub != "user-1" {
			t.Fatalf("known key during outage: %q %v", sub, err)
		}
	}
	if _, err := v.Validate(ctx, unknown()); err != ErrInvalidToken {
		t.Fatalf("unknown key during cooldown: %v", err)
	}
	if got := o.fetches.Load(); got != 2 {
		t.Fatalf("fetches during cooldown = %d, want 2", got)
	}
	// Claim checks still apply to a cached key during the cooldown.
	expired := outageClaims(o.f, now)
	expired["exp"] = now.Add(-time.Hour).Unix()
	if _, err := v.Validate(ctx, o.f.sign(t, "at+jwt", expired)); err == nil {
		t.Fatal("expired token accepted during cooldown")
	}
	// After the cooldown the next validation fetches again; with the
	// endpoint healthy a token with a new key validates.
	now = now.Add(jwksMinRefresh)
	o.extra.Store(&other)
	o.mode.Store(0)
	if sub, err := v.Validate(ctx, unknown()); err != nil || sub != "user-1" {
		t.Fatalf("new key after cooldown: %q %v", sub, err)
	}
	if got := o.fetches.Load(); got != 3 {
		t.Fatalf("fetches after cooldown = %d, want 3", got)
	}
}

func TestLogtoValidatorBacksOffEmptyKeySet(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	o := &jwksOutage{f: tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"}}
	o.mode.Store(2)
	srv := httptest.NewServer(o)
	defer srv.Close()
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://tide.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
	ctx := context.Background()
	token := func() string { return o.f.sign(t, "at+jwt", outageClaims(o.f, now)) }
	for i := 0; i < 3; i++ {
		if _, err := v.Validate(ctx, token()); err != ErrInvalidToken {
			t.Fatalf("validation %d with no key set: %v", i, err)
		}
	}
	if got := o.fetches.Load(); got != 1 {
		t.Fatalf("fetches within cooldown = %d, want 1", got)
	}
	now = now.Add(jwksMinRefresh)
	o.mode.Store(0)
	if _, err := v.Validate(ctx, token()); err != nil {
		t.Fatalf("recovered key set: %v", err)
	}
	if got := o.fetches.Load(); got != 2 {
		t.Fatalf("fetches after cooldown = %d, want 2", got)
	}
}

func TestLogtoValidatorCanceledCallerDoesNotArmCooldown(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	o := &jwksOutage{f: tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"}}
	srv := httptest.NewServer(o)
	defer srv.Close()
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://tide.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
	token := o.f.sign(t, "at+jwt", outageClaims(o.f, now))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := v.Validate(canceled, token); err != ErrInvalidToken {
		t.Fatalf("canceled caller: %v", err)
	}
	if got := o.fetches.Load(); got != 0 {
		t.Fatalf("fetches for canceled caller = %d, want 0", got)
	}
	if sub, err := v.Validate(context.Background(), token); err != nil || sub != "user-1" {
		t.Fatalf("live caller after canceled one: %q %v", sub, err)
	}
}

func TestLogtoValidatorCallerCancelDoesNotAbortFetch(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	o := &jwksOutage{
		f:       tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"},
		block:   make(chan struct{}),
		started: make(chan struct{}, 1),
	}
	srv := httptest.NewServer(o)
	defer srv.Close()
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://tide.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
	token := o.f.sign(t, "at+jwt", outageClaims(o.f, now))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := v.Validate(ctx, token)
		done <- err
	}()
	<-o.started
	cancel()
	// The shared fetch keeps running after the caller left.
	time.Sleep(50 * time.Millisecond)
	close(o.block)
	<-done
	if sub, err := v.Validate(context.Background(), token); err != nil || sub != "user-1" {
		t.Fatalf("live caller after mid-flight cancel: %q %v", sub, err)
	}
	if got := o.fetches.Load(); got != 1 {
		t.Fatalf("fetches = %d, want 1", got)
	}
}
