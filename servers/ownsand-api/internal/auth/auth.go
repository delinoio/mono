// SPDX-License-Identifier: FSL-1.1-ALv2

// Package auth authenticates the bearer credential of each request. It
// selects exactly one validator from the credential shape (osk_ API key, osd_
// device session credential or a Logto JWT access token) and never tries a
// second validator after the first one rejects the credential.
package auth

import (
	"context"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/ratelimit"
	"github.com/delinoio/mono/servers/ownsand-api/internal/secret"
)

// Principal is the authenticated caller.
type Principal struct {
	Kind ownsandv1.PrincipalKind
	// Logto sub for USER, API key ID for API_KEY, device ID for DEVICE.
	ID string
	// Organization of an API key or a device; empty for a user.
	OrganizationID string
	// Expiry of a device session credential.
	ExpiresAt time.Time
}

// Proto returns the protobuf principal.
func (p Principal) Proto() *ownsandv1.Principal {
	return &ownsandv1.Principal{Kind: p.Kind, Id: p.ID}
}

// Same reports whether two principals are the same caller (the same Logto sub
// or the same API key).
func (p Principal) Same(o Principal) bool {
	return p.Kind == o.Kind && p.ID == o.ID
}

// System is the principal of actions that no caller asked for.
var System = Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_SYSTEM}

type principalKey struct{}

// WithPrincipal stores the principal in the context.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// FromContext returns the principal of the request.
func FromContext(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}

// ErrNotFound is returned by CredentialStore lookups for unknown hashes.
var ErrNotFound = errors.New("credential not found")

// APIKeyRecord is the stored state of an API key.
type APIKeyRecord struct {
	APIKeyID       string
	OrganizationID string
	SecretHash     []byte
	Revoked        bool
	OrgDeleted     bool
}

// DeviceSessionRecord is the stored state of a device session credential.
type DeviceSessionRecord struct {
	DeviceID       string
	OrganizationID string
	CredentialHash []byte
	ExpiresAt      time.Time
	DeviceRemoved  bool
}

// CredentialStore looks up stored credentials by their SHA-256 hash.
type CredentialStore interface {
	LookupAPIKey(ctx context.Context, hash []byte) (APIKeyRecord, error)
	LookupDeviceSession(ctx context.Context, hash []byte) (DeviceSessionRecord, error)
}

// TokenValidator validates Logto access tokens.
type TokenValidator interface {
	Validate(ctx context.Context, token string) (UserClaims, error)
}

// Authenticator turns a bearer credential into a principal.
type Authenticator struct {
	Store  CredentialStore
	Tokens TokenValidator
	// Failures limits API key and device credential validation failures per
	// credential display prefix.
	Failures *ratelimit.Limiter
	Now      func() time.Time
}

// Errors returned by Authenticate. They carry no credential material.
var (
	ErrMissingCredential = connect.NewError(connect.CodeUnauthenticated, errors.New("missing bearer credential"))
	ErrInvalidCredential = connect.NewError(connect.CodeUnauthenticated, errors.New("credential is not valid"))
	errRateLimited       = connect.NewError(connect.CodeResourceExhausted, errors.New("too many failed credential validations; retry later"))
)

// BearerToken extracts the bearer credential from an Authorization header.
func BearerToken(header string) (string, bool) {
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

// Authenticate validates the credential. It returns a Connect error with
// CodeUnauthenticated for a missing or invalid credential.
func (a *Authenticator) Authenticate(ctx context.Context, credential string) (Principal, error) {
	now := a.now()
	switch {
	case secret.HasPrefix(credential, secret.KindAPIKey):
		return a.apiKey(ctx, credential)
	case secret.HasPrefix(credential, secret.KindDeviceSession):
		return a.deviceSession(ctx, credential, now)
	case secret.HasPrefix(credential, secret.KindEnrollmentToken), secret.HasPrefix(credential, secret.KindInvitation):
		// Enrollment tokens and invitation secrets are never bearer credentials.
		return Principal{}, ErrInvalidCredential
	case strings.Count(credential, ".") == 2:
		if a.Tokens == nil {
			return Principal{}, ErrInvalidCredential
		}
		claims, err := a.Tokens.Validate(ctx, credential)
		if err != nil {
			return Principal{}, connect.NewError(connect.CodeUnauthenticated, err)
		}
		return Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, ID: claims.Subject}, nil
	default:
		return Principal{}, ErrInvalidCredential
	}
}

func (a *Authenticator) now() time.Time {
	if a.Now != nil {
		return a.Now()
	}
	return time.Now()
}

func (a *Authenticator) limited(credential string) bool {
	return a.Failures != nil && !a.Failures.Peek("prefix:"+secret.DisplayPrefix(credential))
}

func (a *Authenticator) fail(credential string) {
	if a.Failures != nil {
		a.Failures.Allow("prefix:" + secret.DisplayPrefix(credential))
	}
}

func (a *Authenticator) apiKey(ctx context.Context, credential string) (Principal, error) {
	if a.limited(credential) {
		return Principal{}, errRateLimited
	}
	if secret.Classify(credential) != secret.KindAPIKey {
		a.fail(credential)
		return Principal{}, ErrInvalidCredential
	}
	hash := secret.Hash(credential)
	rec, err := a.Store.LookupAPIKey(ctx, hash)
	if errors.Is(err, ErrNotFound) {
		a.fail(credential)
		return Principal{}, ErrInvalidCredential
	}
	if err != nil {
		return Principal{}, connect.NewError(connect.CodeUnavailable, errors.New("credential store unavailable"))
	}
	if !secret.Equal(rec.SecretHash, hash) || rec.Revoked || rec.OrgDeleted {
		a.fail(credential)
		return Principal{}, ErrInvalidCredential
	}
	return Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: rec.APIKeyID, OrganizationID: rec.OrganizationID}, nil
}

func (a *Authenticator) deviceSession(ctx context.Context, credential string, now time.Time) (Principal, error) {
	if a.limited(credential) {
		return Principal{}, errRateLimited
	}
	if secret.Classify(credential) != secret.KindDeviceSession {
		a.fail(credential)
		return Principal{}, ErrInvalidCredential
	}
	hash := secret.Hash(credential)
	rec, err := a.Store.LookupDeviceSession(ctx, hash)
	if errors.Is(err, ErrNotFound) {
		a.fail(credential)
		return Principal{}, ErrInvalidCredential
	}
	if err != nil {
		return Principal{}, connect.NewError(connect.CodeUnavailable, errors.New("credential store unavailable"))
	}
	if !secret.Equal(rec.CredentialHash, hash) || !now.Before(rec.ExpiresAt) || rec.DeviceRemoved {
		a.fail(credential)
		return Principal{}, ErrInvalidCredential
	}
	return Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE, ID: rec.DeviceID, OrganizationID: rec.OrganizationID, ExpiresAt: rec.ExpiresAt}, nil
}
