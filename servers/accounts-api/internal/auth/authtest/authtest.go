// SPDX-License-Identifier: FSL-1.1-ALv2

// Package authtest is a test Logto issuer: it signs access tokens and ID
// tokens with an ES256 key and serves the key set.
package authtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// Audience is the accounts-api API resource indicator.
const Audience = "https://accounts.api.delino.io"

// Issuer signs tokens and serves its key set over HTTP.
type Issuer struct {
	// URL is the issuer (`iss`), `<server>/oidc`.
	URL     string
	JWKSURL string
	key     *ecdsa.PrivateKey
	keyID   string
	server  *httptest.Server
}

// NewIssuer starts an issuer that stops when the test ends.
func NewIssuer(t testing.TB) *Issuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{key: key, keyID: "test-key"}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: iss.keyID, Algorithm: string(jose.ES256), Use: "sig"}}}
	body, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	iss.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/oidc/jwks" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(iss.server.Close)
	iss.URL = iss.server.URL + "/oidc"
	iss.JWKSURL = iss.URL + "/jwks"
	return iss
}

// Claims describe a token.
type Claims struct {
	Subject  string
	ClientID string
	Audience string
	Scopes   []string
	Expiry   time.Time
	// Type is the JWT `typ` header; empty selects `at+jwt`.
	Type string
}

// Sign returns a signed token with the claims.
func (i *Issuer) Sign(t testing.TB, c Claims) string {
	t.Helper()
	typ := c.Type
	if typ == "" {
		typ = "at+jwt"
	}
	opts := (&jose.SignerOptions{}).WithType(jose.ContentType(typ)).WithHeader(jose.HeaderKey("kid"), i.keyID)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: i.key}, opts)
	if err != nil {
		t.Fatal(err)
	}
	audience := c.Audience
	if audience == "" {
		audience = Audience
	}
	expiry := c.Expiry
	if expiry.IsZero() {
		expiry = time.Now().Add(time.Hour)
	}
	claims := map[string]any{
		"iss":   i.URL,
		"sub":   c.Subject,
		"aud":   audience,
		"exp":   jwt.NewNumericDate(expiry),
		"iat":   jwt.NewNumericDate(time.Now()),
		"scope": strings.Join(c.Scopes, " "),
	}
	if c.ClientID != "" {
		claims["client_id"] = c.ClientID
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// UserToken returns an access token of the user with the scope
// `accounts:billing`.
func (i *Issuer) UserToken(t testing.TB, userID string) string {
	t.Helper()
	return i.Sign(t, Claims{Subject: userID, ClientID: "web-app", Scopes: []string{"openid", "accounts:billing"}})
}

// IDToken returns an OpenID Connect ID token of the user: `typ` JWT and the
// client ID as audience.
func (i *Issuer) IDToken(t testing.TB, userID string) string {
	t.Helper()
	return i.Sign(t, Claims{Subject: userID, Audience: "web-app", Type: "JWT", Scopes: []string{"accounts:billing"}})
}

// MachineToken returns a client credentials token of the application with
// the scopes.
func (i *Issuer) MachineToken(t testing.TB, appID string, scopes ...string) string {
	t.Helper()
	return i.Sign(t, Claims{Subject: appID, ClientID: appID, Scopes: scopes})
}
