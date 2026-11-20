// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

const (
	testIssuer   = "https://tenant.example/oidc"
	testAudience = "https://ownsand.api.delino.io"
)

var testNow = time.Unix(1_800_000_000, 0)

func b64(v any) string {
	b, _ := json.Marshal(v)
	return base64.RawURLEncoding.EncodeToString(b)
}

func signES384(t *testing.T, key *ecdsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	signed := b64(header) + "." + b64(claims)
	sum := sha512.Sum384([]byte(signed))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	sig := make([]byte, 96)
	r.FillBytes(sig[:48])
	s.FillBytes(sig[48:])
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func signRS256(t *testing.T, key *rsa.PrivateKey, header, claims map[string]any) string {
	t.Helper()
	signed := b64(header) + "." + b64(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func goodClaims() map[string]any {
	return map[string]any{
		"iss": testIssuer, "aud": testAudience, "sub": "user-1", "client_id": "app-1",
		"exp": testNow.Add(time.Hour).Unix(), "iat": testNow.Unix(), "scope": "openid " + RequiredScope,
	}
}

func TestJWTValidation(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	otherKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	v := newJWTValidatorWithKeys(testIssuer, testAudience, map[string]crypto.PublicKey{"ec": &ecKey.PublicKey, "rsa": &rsaKey.PublicKey}, func() time.Time { return testNow })
	hdr := map[string]any{"alg": "ES384", "kid": "ec", "typ": "at+jwt"}
	with := func(mut func(map[string]any)) map[string]any {
		c := goodClaims()
		mut(c)
		return c
	}
	cases := []struct {
		name  string
		token string
		want  error
	}{
		{"valid ES384", signES384(t, ecKey, hdr, goodClaims()), nil},
		{"valid RS256", signRS256(t, rsaKey, map[string]any{"alg": "RS256", "kid": "rsa"}, goodClaims()), nil},
		{"audience array", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["aud"] = []string{"x", testAudience} })), nil},
		{"expired", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["exp"] = testNow.Add(-time.Hour).Unix() })), errExpired},
		{"no exp", signES384(t, ecKey, hdr, with(func(c map[string]any) { delete(c, "exp") })), errExpired},
		{"not yet valid", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["nbf"] = testNow.Add(time.Hour).Unix() })), errNotYetValid},
		{"wrong issuer", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["iss"] = "https://evil/oidc" })), errIssuer},
		{"wrong audience (ID token)", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["aud"] = "app-1" })), errAudience},
		{"ID token typ", signES384(t, ecKey, map[string]any{"alg": "ES384", "kid": "ec", "typ": "JWT"}, goodClaims()), errNotAccessToken},
		{"missing scope", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["scope"] = "openid" })), errScope},
		{"machine-to-machine", signES384(t, ecKey, hdr, with(func(c map[string]any) { c["sub"] = "app-1" })), errMachineToken},
		{"no client_id", signES384(t, ecKey, hdr, with(func(c map[string]any) { delete(c, "client_id") })), errMachineToken},
		{"unknown key", signES384(t, otherKey, map[string]any{"alg": "ES384", "kid": "nope"}, goodClaims()), errUnknownKey},
		{"bad signature", signES384(t, otherKey, hdr, goodClaims()), errBadSignature},
		{"alg confusion", signES384(t, ecKey, map[string]any{"alg": "RS256", "kid": "ec"}, goodClaims()), errBadSignature},
		{"alg none", b64(map[string]any{"alg": "none", "kid": "ec"}) + "." + b64(goodClaims()) + ".", errBadSignature},
		{"malformed", "abc.def", errMalformedToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := v.Validate(context.Background(), tc.token)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
			if err == nil && claims.Subject != "user-1" {
				t.Fatalf("subject %q", claims.Subject)
			}
		})
	}
}

func TestJWKSFetchAndParse(t *testing.T) {
	ecKey, _ := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	pad := func(b []byte, n int) []byte { out := make([]byte, n); copy(out[n-len(b):], b); return out }
	ecdh, _ := ecKey.PublicKey.Bytes()
	x, y := ecdh[1:49], ecdh[49:]
	jwks := map[string]any{"keys": []map[string]any{
		{"kty": "EC", "kid": "ec", "crv": "P-384", "x": base64.RawURLEncoding.EncodeToString(pad(x, 48)), "y": base64.RawURLEncoding.EncodeToString(pad(y, 48)), "use": "sig"},
		{"kty": "RSA", "kid": "rsa", "n": base64.RawURLEncoding.EncodeToString(rsaKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(rsaKey.E)).Bytes())},
		{"kty": "oct", "kid": "skip", "k": "AAAA"},
	}}
	fetches := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		_ = json.NewEncoder(w).Encode(jwks)
	}))
	defer srv.Close()
	v := NewJWTValidator(testIssuer, srv.URL, testAudience, srv.Client(), nil)
	v.now = func() time.Time { return testNow }
	for _, tok := range []string{
		signES384(t, ecKey, map[string]any{"alg": "ES384", "kid": "ec"}, goodClaims()),
		signRS256(t, rsaKey, map[string]any{"alg": "RS256", "kid": "rsa"}, goodClaims()),
	} {
		if _, err := v.Validate(context.Background(), tok); err != nil {
			t.Fatalf("validate: %v", err)
		}
	}
	if fetches != 1 {
		t.Fatalf("JWKS fetched %d times, want 1", fetches)
	}
	_ = fmt.Sprint()
}
