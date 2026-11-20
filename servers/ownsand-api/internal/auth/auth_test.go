// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/uuidv7"
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
