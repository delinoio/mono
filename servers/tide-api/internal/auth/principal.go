// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import "context"

// PrincipalKind is the kind of credential that authenticated a caller.
type PrincipalKind int

const (
	PrincipalUnspecified PrincipalKind = iota
	// PrincipalUser authenticated with a Logto access token.
	PrincipalUser
	// PrincipalAPIKey authenticated with a personal API key. It acts as the
	// key's user in the key's organization only.
	PrincipalAPIKey
)

// String returns the stable log value of the kind.
func (k PrincipalKind) String() string {
	switch k {
	case PrincipalUser:
		return "user"
	case PrincipalAPIKey:
		return "api_key"
	}
	return "unspecified"
}

// Principal is an authenticated caller. Every principal acts as a user:
// a personal API key acts as the user who created it.
type Principal struct {
	Kind PrincipalKind
	// UserID is the Logto `sub` of the user, also for a personal API key.
	UserID string
	// APIKeyID and OrganizationID are set only for a personal API key.
	APIKeyID       string
	OrganizationID string
}

// IsUser reports whether the principal used a Logto access token.
func (p Principal) IsUser() bool { return p.Kind == PrincipalUser }

// IsAPIKey reports whether the principal used a personal API key.
func (p Principal) IsAPIKey() bool { return p.Kind == PrincipalAPIKey }

type principalKey struct{}

// WithPrincipal stores the principal in ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal of ctx.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(Principal)
	return p, ok
}
