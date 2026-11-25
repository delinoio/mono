// SPDX-License-Identifier: FSL-1.1-ALv2

package auth

import (
	"context"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Principal is an authenticated caller.
type Principal struct {
	Kind ownsandv1.PrincipalKind
	// ID is the Logto `sub` for a user, the API key ID for an API key and
	// the device ID for a device.
	ID string
	// OrganizationID is set for API keys and devices, which belong to one
	// organization.
	OrganizationID string
}

// MembershipSnapshot is the membership and last control event visible when
// a user was authorized for data path work. It is empty for API keys.
type MembershipSnapshot struct {
	ID         string
	DeliveryID int64
}

// Proto returns the protobuf form of the principal.
func (p Principal) Proto() *ownsandv1.Principal {
	return &ownsandv1.Principal{Kind: p.Kind, Id: p.ID}
}

// IsUser reports whether the principal is a Logto user.
func (p Principal) IsUser() bool { return p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER }

// IsAPIKey reports whether the principal is an organization API key.
func (p Principal) IsAPIKey() bool { return p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY }

// IsDevice reports whether the principal is a device.
func (p Principal) IsDevice() bool { return p.Kind == ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE }

// SystemPrincipal is the actor of actions that no caller requested.
var SystemPrincipal = Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_SYSTEM}

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
