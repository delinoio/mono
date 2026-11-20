// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

// RequiredScope is the Logto permission (scope) of the OwnSand API resource
// that every user RPC requires. OwnSand's own roles authorize the action; the
// scope only marks a token that was issued for OwnSand use. No
// machine-to-machine application gets this scope.
const RequiredScope = "ownsand:access"

// Validation errors. The message never contains the token.
var (
	errMalformedToken = errors.New("malformed access token")
	errBadSignature   = errors.New("access token signature is not valid")
	errUnknownKey     = errors.New("access token signing key is unknown")
	errExpired        = errors.New("access token expired")
	errNotYetValid    = errors.New("access token is not valid yet")
	errIssuer         = errors.New("access token issuer does not match")
	errAudience       = errors.New("access token audience does not match")
	errScope          = errors.New("access token lacks the required scope")
	errNotAccessToken = errors.New("token is not an access token")
	errMachineToken   = errors.New("machine-to-machine tokens are not accepted")
	errNoSubject      = errors.New("access token has no subject")
)

// clockSkew is the tolerance for exp and nbf.
const clockSkew = 30 * time.Second

// JWTValidator validates Logto access tokens with the tenant's JWKS keys,
// using only the Go standard library.
type JWTValidator struct {
	Issuer   string
	Audience string
	keys     *jwksCache
	now      func() time.Time
}

// NewJWTValidator returns a validator for the tenant issuer, its JWKS URI and
// the API resource indicator.
func NewJWTValidator(issuer, jwksURI, audience string, client *http.Client, logger *slog.Logger) *JWTValidator {
	return &JWTValidator{
		Issuer:   issuer,
		Audience: audience,
		keys:     &jwksCache{uri: jwksURI, client: client, logger: logger, keys: map[string]crypto.PublicKey{}},
		now:      time.Now,
	}
}

// newJWTValidatorWithKeys builds a validator with fixed keys for tests.
func newJWTValidatorWithKeys(issuer, audience string, keys map[string]crypto.PublicKey, now func() time.Time) *JWTValidator {
	return &JWTValidator{Issuer: issuer, Audience: audience, keys: &jwksCache{keys: keys, static: true}, now: now}
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

type jwtClaims struct {
	Iss      string          `json:"iss"`
	Sub      string          `json:"sub"`
	Aud      json.RawMessage `json:"aud"`
	Exp      *float64        `json:"exp"`
	Nbf      *float64        `json:"nbf"`
	Scope    string          `json:"scope"`
	ClientID string          `json:"client_id"`
}

// UserClaims are the validated claims of a user access token.
type UserClaims struct {
	Subject string
	Scopes  []string
}

// Validate checks the signature, issuer, expiry, audience and scope of a
// Logto access token and that it belongs to an end user.
//
// Machine-to-machine tokens are rejected: Logto sets sub to the application
// ID in such a token, so a token whose sub equals its client_id (or that has
// no client_id) is not an end-user token. ID tokens are rejected because
// their audience is the client ID, not the API resource, and because their
// typ header is not "at+jwt".
func (v *JWTValidator) Validate(ctx context.Context, token string) (UserClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return UserClaims{}, errMalformedToken
	}
	var header jwtHeader
	if err := decodeSegment(parts[0], &header); err != nil {
		return UserClaims{}, errMalformedToken
	}
	if header.Typ != "" && !strings.EqualFold(header.Typ, "at+jwt") && !strings.EqualFold(header.Typ, "application/at+jwt") {
		return UserClaims{}, errNotAccessToken
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return UserClaims{}, errMalformedToken
	}
	key, err := v.keys.get(ctx, header.Kid)
	if err != nil {
		return UserClaims{}, err
	}
	if err := verifySignature(header.Alg, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return UserClaims{}, err
	}
	var claims jwtClaims
	if err := decodeSegment(parts[1], &claims); err != nil {
		return UserClaims{}, errMalformedToken
	}
	now := v.now()
	if claims.Exp == nil || now.After(time.Unix(int64(*claims.Exp), 0).Add(clockSkew)) {
		return UserClaims{}, errExpired
	}
	if claims.Nbf != nil && now.Add(clockSkew).Before(time.Unix(int64(*claims.Nbf), 0)) {
		return UserClaims{}, errNotYetValid
	}
	if claims.Iss != v.Issuer {
		return UserClaims{}, errIssuer
	}
	if !audienceMatches(claims.Aud, v.Audience) {
		return UserClaims{}, errAudience
	}
	if claims.Sub == "" {
		return UserClaims{}, errNoSubject
	}
	if claims.ClientID == "" || claims.ClientID == claims.Sub {
		return UserClaims{}, errMachineToken
	}
	scopes := strings.Fields(claims.Scope)
	found := false
	for _, s := range scopes {
		if s == RequiredScope {
			found = true
		}
	}
	if !found {
		return UserClaims{}, errScope
	}
	return UserClaims{Subject: claims.Sub, Scopes: scopes}, nil
}

func decodeSegment(seg string, out any) error {
	b, err := base64.RawURLEncoding.DecodeString(seg)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	return dec.Decode(out)
}

func audienceMatches(raw json.RawMessage, want string) bool {
	if len(raw) == 0 {
		return false
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return one == want
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		for _, a := range many {
			if a == want {
				return true
			}
		}
	}
	return false
}

func verifySignature(alg string, key crypto.PublicKey, signed, sig []byte) error {
	var h hash.Hash
	var ch crypto.Hash
	switch alg {
	case "RS256", "ES256", "PS256":
		h, ch = sha256.New(), crypto.SHA256
	case "RS384", "ES384", "PS384":
		h, ch = sha512.New384(), crypto.SHA384
	case "RS512", "ES512", "PS512":
		h, ch = sha512.New(), crypto.SHA512
	case "EdDSA":
		k, ok := key.(ed25519.PublicKey)
		if !ok || !ed25519.Verify(k, signed, sig) {
			return errBadSignature
		}
		return nil
	default:
		// "none" and every other algorithm are rejected.
		return errBadSignature
	}
	h.Write(signed)
	digest := h.Sum(nil)
	switch alg[:2] {
	case "RS":
		k, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPKCS1v15(k, ch, digest, sig) != nil {
			return errBadSignature
		}
	case "PS":
		k, ok := key.(*rsa.PublicKey)
		if !ok || rsa.VerifyPSS(k, ch, digest, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) != nil {
			return errBadSignature
		}
	case "ES":
		k, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return errBadSignature
		}
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return errBadSignature
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(k, digest, r, s) {
			return errBadSignature
		}
	}
	return nil
}

// jwksCache holds the tenant's signing keys. It refreshes on an unknown key
// ID, at most once per jwksMinRefresh, and after jwksMaxAge.
type jwksCache struct {
	uri     string
	client  *http.Client
	logger  *slog.Logger
	static  bool
	mu      sync.Mutex
	keys    map[string]crypto.PublicKey
	fetched time.Time
}

const (
	jwksMinRefresh = 30 * time.Second
	jwksMaxAge     = 10 * time.Minute
)

func (c *jwksCache) get(ctx context.Context, kid string) (crypto.PublicKey, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.static {
		if k, ok := c.keys[kid]; ok {
			return k, nil
		}
		return nil, errUnknownKey
	}
	k, ok := c.keys[kid]
	stale := time.Since(c.fetched) > jwksMaxAge
	if ok && !stale {
		return k, nil
	}
	if time.Since(c.fetched) >= jwksMinRefresh || stale {
		if err := c.refresh(ctx); err != nil {
			c.logger.Warn("jwks_refresh_failed", slog.Any("error", err))
		}
		k, ok = c.keys[kid]
	}
	if !ok {
		return nil, errUnknownKey
	}
	return k, nil
}

func (c *jwksCache) refresh(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.uri, nil)
	if err != nil {
		return err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	keys, err := ParseJWKS(body)
	if err != nil {
		return err
	}
	c.keys = keys
	c.fetched = time.Now()
	return nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// ParseJWKS parses a JSON Web Key Set into public keys by key ID. Keys of
// unsupported types are skipped.
func ParseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &set); err != nil {
		return nil, err
	}
	out := map[string]crypto.PublicKey{}
	for _, k := range set.Keys {
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		key, err := k.publicKey()
		if err != nil {
			continue
		}
		out[k.Kid] = key
	}
	return out, nil
}

func (k jwk) publicKey() (crypto.PublicKey, error) {
	dec := base64.RawURLEncoding
	switch k.Kty {
	case "RSA":
		n, err := dec.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		e, err := dec.DecodeString(k.E)
		if err != nil || len(e) > 4 {
			return nil, errors.New("bad exponent")
		}
		var exp int
		for _, b := range e {
			exp = exp<<8 | int(b)
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exp}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, errors.New("unsupported curve")
		}
		x, err := dec.DecodeString(k.X)
		if err != nil {
			return nil, err
		}
		y, err := dec.DecodeString(k.Y)
		if err != nil {
			return nil, err
		}
		size := (curve.Params().BitSize + 7) / 8
		if len(x) != size || len(y) != size {
			return nil, errors.New("bad point size")
		}
		uncompressed := append(append([]byte{4}, x...), y...)
		return ecdsa.ParseUncompressedPublicKey(curve, uncompressed)
	case "OKP":
		if k.Crv != "Ed25519" {
			return nil, errors.New("unsupported curve")
		}
		x, err := dec.DecodeString(k.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad key")
		}
		return ed25519.PublicKey(x), nil
	default:
		return nil, errors.New("unsupported key type")
	}
}
