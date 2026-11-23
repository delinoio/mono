// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/delinoio/mono/go/uuidv7"
)

func TestSecrets(t *testing.T) {
	for _, kind := range []SecretKind{SecretAPIKey, SecretEnrollmentToken, SecretInvitation, SecretDeviceSession} {
		s, err := NewSecret(kind)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(s.Value, kind.Prefix()) || len(s.Value) != 4+43 {
			t.Fatalf("%v: shape of %d bytes", kind, len(s.Value))
		}
		if ClassifySecret(s.Value) != kind || !WellFormed(kind, s.Value) {
			t.Fatalf("%v: not recognized", kind)
		}
		if !HashEqual(s.Hash, HashSecret(s.Value)) || s.DisplayPrefix != s.Value[:8] {
			t.Fatalf("%v: hash or prefix mismatch", kind)
		}
	}
	if ClassifySecret("eyJhbGciOi") != SecretUnknown {
		t.Fatal("a JWT is not an OwnSand secret")
	}
	if WellFormed(SecretAPIKey, "osk_short") || WellFormed(SecretAPIKey, "osk_"+strings.Repeat("=", 43)) {
		t.Fatal("malformed key accepted")
	}
}

func TestNonces(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	clock := func() time.Time { return now }
	key := []byte(strings.Repeat("k", 32))
	issuer := NewNonceIssuer(key, nil, clock)
	device, _ := uuidv7.NewString()
	other, _ := uuidv7.NewString()
	nonce, expiry, err := issuer.Issue(device)
	if err != nil || !expiry.Equal(now.Add(NonceLifetime)) {
		t.Fatalf("issue: %v %v", expiry, err)
	}
	if _, err := issuer.Check(device, nonce); err != nil {
		t.Fatalf("check: %v", err)
	}
	if _, err := issuer.Check(other, nonce); err == nil {
		t.Fatal("nonce accepted for another device")
	}
	tampered := append([]byte(nil), nonce...)
	tampered[20] ^= 1
	if _, err := issuer.Check(device, tampered); err == nil {
		t.Fatal("tampered nonce accepted")
	}
	// Rotation: the new key accepts nonces of the previous key.
	rotated := NewNonceIssuer([]byte(strings.Repeat("n", 32)), key, clock)
	if _, err := rotated.Check(device, nonce); err != nil {
		t.Fatalf("previous key: %v", err)
	}
	if _, err := NewNonceIssuer([]byte(strings.Repeat("n", 32)), nil, clock).Check(device, nonce); err == nil {
		t.Fatal("nonce of an unknown key accepted")
	}
	now = now.Add(NonceLifetime)
	if _, err := issuer.Check(device, nonce); err == nil {
		t.Fatal("expired nonce accepted")
	}
}

func TestDeviceSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	nonce := []byte("nonce")
	sig := ed25519.Sign(priv, append([]byte(PurposeAuthenticate), nonce...))
	if !VerifyDeviceSignature(pub, PurposeAuthenticate, nonce, sig) {
		t.Fatal("valid signature rejected")
	}
	if VerifyDeviceSignature(pub, PurposeConfirmRemoval, nonce, sig) {
		t.Fatal("signature accepted for another purpose")
	}
	if VerifyDeviceSignature(pub[:5], PurposeAuthenticate, nonce, sig) {
		t.Fatal("short key accepted")
	}
}

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
	v := NewLogtoValidator(LogtoConfig{Issuer: f.issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: jwks.URL, Now: func() time.Time { return now }})
	base := func() map[string]any {
		return map[string]any{
			"iss": f.issuer, "aud": "https://ownsand.api.delino.io", "sub": "user-1", "client_id": "app-1",
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
	v := NewLogtoValidator(LogtoConfig{Issuer: f.issuer, Audience: "https://ownsand.api.delino.io", JWKSFile: path})
	token := f.sign(t, "at+jwt", map[string]any{
		"iss": f.issuer, "aud": []string{"https://ownsand.api.delino.io"}, "sub": "e2e-user",
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
		"iss": f.issuer, "aud": "https://ownsand.api.delino.io", "sub": "user-1",
		"exp": now.Add(24 * time.Hour).Unix(), "scope": RequiredScope,
	}
}

func TestLogtoValidatorBacksOffFailedRefresh(t *testing.T) {
	key, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	o := &jwksOutage{f: tokenFactory{key: key, kid: "k1", issuer: "https://tenant.logto.app/oidc"}}
	srv := httptest.NewServer(o)
	defer srv.Close()
	now := time.Now()
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
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
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
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
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
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
	v := NewLogtoValidator(LogtoConfig{Issuer: o.f.issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: srv.URL, Now: func() time.Time { return now }})
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

// fakeClock is a clock that tests advance while key set fetches run in
// other goroutines.
type fakeClock struct{ ns atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.ns.Store(time.Now().UnixNano())
	return c
}

func (c *fakeClock) Now() time.Time          { return time.Unix(0, c.ns.Load()) }
func (c *fakeClock) Advance(d time.Duration) { c.ns.Add(int64(d)) }

// parkedJWKS serves a key set whose handler a test can park: while parked,
// a request does not answer until the test releases it or the client gives
// up. It reads its switches atomically because the handler runs in another
// goroutine than the test.
type parkedJWKS struct {
	keys    []tokenFactory
	fetches atomic.Int32
	parked  atomic.Bool
	release chan struct{}
	started chan struct{}
	once    sync.Once
}

func newParkedJWKS(keys ...tokenFactory) *parkedJWKS {
	return &parkedJWKS{keys: keys, release: make(chan struct{}), started: make(chan struct{}, 16)}
}

func (p *parkedJWKS) Release() { p.once.Do(func() { close(p.release) }) }

func (p *parkedJWKS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.fetches.Add(1)
	p.started <- struct{}{}
	if p.parked.Load() {
		select {
		case <-p.release:
		case <-r.Context().Done():
			return
		}
	}
	set := jose.JSONWebKeySet{}
	for _, f := range p.keys {
		set.Keys = append(set.Keys, jose.JSONWebKey{Key: &f.key.PublicKey, KeyID: f.kid, Algorithm: string(jose.ES384), Use: "sig"})
	}
	b, _ := json.Marshal(set)
	_, _ = w.Write(b)
}

type parkedEnv struct {
	p     *parkedJWKS
	v     *LogtoValidator
	clock *fakeClock
	known tokenFactory
	other tokenFactory
}

// newParkedEnv returns a validator whose key set contains the key "k1" and,
// after the first fetch, "k2" (the key of the token that is not cached yet).
func newParkedEnv(t *testing.T) *parkedEnv {
	t.Helper()
	issuer := "https://tenant.logto.app/oidc"
	k1, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	k2, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	e := &parkedEnv{
		known: tokenFactory{key: k1, kid: "k1", issuer: issuer},
		other: tokenFactory{key: k2, kid: "k2", issuer: issuer},
		clock: newFakeClock(),
	}
	e.p = newParkedJWKS(e.known)
	srv := httptest.NewServer(e.p)
	t.Cleanup(srv.Close)
	// Cleanups run last in, first out: release parked handlers before Close.
	t.Cleanup(e.p.Release)
	e.v = NewLogtoValidator(LogtoConfig{Issuer: issuer, Audience: "https://ownsand.api.delino.io", JWKSURL: srv.URL, Now: e.clock.Now})
	e.v.fetchTimeout = 10 * time.Second
	return e
}

func (e *parkedEnv) token(t *testing.T, f tokenFactory) string {
	t.Helper()
	return f.sign(t, "at+jwt", outageClaims(f, e.clock.Now()))
}

// load fetches the first key set with the one key "k1", lets the server also
// serve "k2" so that a "k2" token is an unknown key ID, and moves the clock
// past the cooldown of the first fetch while the cache stays fresh.
func (e *parkedEnv) load(t *testing.T) {
	t.Helper()
	if _, err := e.v.Validate(context.Background(), e.token(t, e.known)); err != nil {
		t.Fatal(err)
	}
	e.p.keys = append(e.p.keys, e.other)
	<-e.p.started
	// Leave the cooldown of the first fetch.
	e.clock.Advance(jwksMinRefresh)
}

type validateResult struct {
	sub string
	err error
	dur time.Duration
}

func (e *parkedEnv) validateAsync(ctx context.Context, raw string) <-chan validateResult {
	out := make(chan validateResult, 1)
	go func() {
		start := time.Now()
		sub, err := e.v.Validate(ctx, raw)
		out <- validateResult{sub, err, time.Since(start)}
	}()
	return out
}

func waitResult(t *testing.T, ch <-chan validateResult, within time.Duration) validateResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(within):
		t.Fatalf("validation did not return within %s", within)
		return validateResult{}
	}
}

// parkedFetchStartTimeout bounds how long a test waits for the handler of the
// parked JWKS fixture to receive a request.
const parkedFetchStartTimeout = 5 * time.Second

// waitFetchStart waits until the parked JWKS handler receives a request. The
// handler only sends on started when the validator fetches, so a validator that
// returns early (for example with ErrInvalidToken or because of the cooldown)
// would otherwise leave the test blocked until the package timeout. early is the
// pending validation result, or nil when the test has none; a result that
// arrives before the fetch starts, and a missing fetch after
// parkedFetchStartTimeout, fail the test with a message that names the fetch.
func (e *parkedEnv) waitFetchStart(t *testing.T, what string, early <-chan validateResult) {
	t.Helper()
	select {
	case <-e.p.started:
	case r := <-early:
		t.Fatalf("validation returned before the %s fetch started: sub=%q err=%v after %s", what, r.sub, r.err, r.dur)
	case <-time.After(parkedFetchStartTimeout):
		t.Fatalf("the %s fetch did not start within %s", what, parkedFetchStartTimeout)
	}
}

func assertPending(t *testing.T, ch <-chan validateResult) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("validation returned while the fetch was parked: %+v", r)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestLogtoValidatorCachedKeyDoesNotWaitForStaleRefresh(t *testing.T) {
	e := newParkedEnv(t)
	e.load(t)
	e.clock.Advance(jwksRefreshInterval + time.Minute)
	e.p.parked.Store(true)
	// The first caller with a cached key starts the refresh in the
	// background and returns the cached key at once.
	r := waitResult(t, e.validateAsync(context.Background(), e.token(t, e.known)), 2*time.Second)
	if r.err != nil || r.sub != "user-1" {
		t.Fatalf("cached key during stale refresh: %+v", r)
	}
	e.waitFetchStart(t, "stale refresh", nil)
	// More callers with a cached key use the key while the fetch runs, and
	// none of them starts another fetch.
	for i := 0; i < 3; i++ {
		r := waitResult(t, e.validateAsync(context.Background(), e.token(t, e.known)), 2*time.Second)
		if r.err != nil || r.sub != "user-1" {
			t.Fatalf("cached key %d while the fetch runs: %+v", i, r)
		}
	}
	e.p.Release()
	// The stale refresh adds the new key for the next callers.
	r = waitResult(t, e.validateAsync(context.Background(), e.token(t, e.other)), 2*time.Second)
	if r.err != nil || r.sub != "user-1" {
		t.Fatalf("new key after the refresh: %+v", r)
	}
	if got := e.p.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}
}

func TestLogtoValidatorCachedKeyDoesNotWaitForUnknownKeyFetch(t *testing.T) {
	e := newParkedEnv(t)
	e.load(t)
	e.p.parked.Store(true)
	// The caller with the unknown key ID waits for the parked fetch.
	waiter := e.validateAsync(context.Background(), e.token(t, e.other))
	e.waitFetchStart(t, "unknown key", waiter)
	assertPending(t, waiter)
	// The cached key validates at once during the fetch, with a fresh cache.
	r := waitResult(t, e.validateAsync(context.Background(), e.token(t, e.known)), 2*time.Second)
	if r.err != nil || r.sub != "user-1" {
		t.Fatalf("cached key during unknown key fetch: %+v", r)
	}
	// A second caller with the same unknown key ID joins the running fetch.
	second := e.validateAsync(context.Background(), e.token(t, e.other))
	assertPending(t, second)
	e.p.Release()
	for i, ch := range []<-chan validateResult{waiter, second} {
		if r := waitResult(t, ch, 2*time.Second); r.err != nil || r.sub != "user-1" {
			t.Fatalf("waiter %d after the fetch: %+v", i, r)
		}
	}
	if got := e.p.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}
}

func TestLogtoValidatorCanceledWaiterReturnsAndFetchContinues(t *testing.T) {
	e := newParkedEnv(t)
	e.load(t)
	e.p.parked.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	canceled := e.validateAsync(ctx, e.token(t, e.other))
	e.waitFetchStart(t, "canceled waiter unknown key", canceled)
	assertPending(t, canceled)
	cancel()
	r := waitResult(t, canceled, 2*time.Second)
	if r.err != ErrInvalidToken {
		t.Fatalf("canceled waiter: %v", r.err)
	}
	if _, err := e.v.key(ctx, "k2"); !errors.Is(err, context.Canceled) {
		t.Fatalf("key with a canceled context = %v, want context.Canceled", err)
	}
	// The fetch keeps running: a later live caller joins it and sees the new
	// key once the fetch ends.
	live := e.validateAsync(context.Background(), e.token(t, e.other))
	assertPending(t, live)
	e.p.Release()
	if r := waitResult(t, live, 2*time.Second); r.err != nil || r.sub != "user-1" {
		t.Fatalf("live caller after the canceled waiter: %+v", r)
	}
	if got := e.p.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}
}

func TestLogtoValidatorCanceledCallerDoesNotWaitOrArmCooldown(t *testing.T) {
	e := newParkedEnv(t)
	e.load(t)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	// A caller that is already gone neither waits nor starts a fetch for a
	// key ID that is not cached.
	r := waitResult(t, e.validateAsync(canceled, e.token(t, e.other)), 2*time.Second)
	if r.err != ErrInvalidToken {
		t.Fatalf("canceled caller: %v", r.err)
	}
	// It does not start the stale refresh either.
	e.clock.Advance(jwksRefreshInterval + time.Minute)
	r = waitResult(t, e.validateAsync(canceled, e.token(t, e.known)), 2*time.Second)
	if r.err != nil || r.sub != "user-1" {
		t.Fatalf("canceled caller with a cached key: %+v", r)
	}
	time.Sleep(50 * time.Millisecond)
	if got := e.p.fetches.Load(); got != 1 {
		t.Fatalf("fetches for canceled callers = %d, want 1", got)
	}
	// The cooldown is not armed: the next live caller fetches at once.
	if r := waitResult(t, e.validateAsync(context.Background(), e.token(t, e.other)), 2*time.Second); r.err != nil || r.sub != "user-1" {
		t.Fatalf("live caller after canceled ones: %+v", r)
	}
	if got := e.p.fetches.Load(); got != 2 {
		t.Fatalf("fetches = %d, want 2", got)
	}
}

func TestLogtoValidatorHungEndpointBlocksOnlyUncachedKeysForTheFetchTimeout(t *testing.T) {
	e := newParkedEnv(t)
	e.v.fetchTimeout = 300 * time.Millisecond
	e.load(t)
	e.p.parked.Store(true)
	start := time.Now()
	hung := e.validateAsync(context.Background(), e.token(t, e.other))
	e.waitFetchStart(t, "hung endpoint", hung)
	// While the endpoint hangs, callers with a cached key are not blocked.
	r := waitResult(t, e.validateAsync(context.Background(), e.token(t, e.known)), 2*time.Second)
	if r.err != nil || r.sub != "user-1" {
		t.Fatalf("cached key during the hang: %+v", r)
	}
	// The uncached key waits for the fetch timeout and no longer.
	r = waitResult(t, hung, 5*time.Second)
	if r.err != ErrInvalidToken {
		t.Fatalf("uncached key after the hang: %v", r.err)
	}
	if elapsed := time.Since(start); elapsed < 250*time.Millisecond {
		t.Fatalf("uncached key returned after %s, before the fetch timeout", elapsed)
	}
	// Within the cooldown no caller fetches or waits again.
	r = waitResult(t, e.validateAsync(context.Background(), e.token(t, e.other)), time.Second)
	if r.err != ErrInvalidToken || r.dur > 200*time.Millisecond {
		t.Fatalf("uncached key during the cooldown: %+v", r)
	}
	if got := e.p.fetches.Load(); got != 2 {
		t.Fatalf("fetches within the cooldown = %d, want 2", got)
	}
	// One minute after the attempt the next uncached key fetches again.
	e.clock.Advance(jwksMinRefresh)
	r = waitResult(t, e.validateAsync(context.Background(), e.token(t, e.other)), 5*time.Second)
	if r.err != ErrInvalidToken {
		t.Fatalf("uncached key after the cooldown: %v", r.err)
	}
	if got := e.p.fetches.Load(); got != 3 {
		t.Fatalf("fetches after the cooldown = %d, want 3", got)
	}
}
