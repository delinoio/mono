// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/servers/tide-api/internal/auth"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
)

// Caller is an authorized caller of an organization-scoped RPC.
type Caller struct {
	// Principal is the authenticated credential. For a personal API key,
	// Principal.APIKeyID is set; audit events record it as the actor key.
	Principal auth.Principal
	// UserID is the Logto `sub` of the acting user (also for an API key).
	UserID string
	// OrganizationID is the canonical organization ID of the request.
	OrganizationID string
	// Role is the user's current role in the organization.
	Role tidev1.Role
	// MembershipID is the user's membership in the organization.
	MembershipID string
}

// principal returns the authenticated principal of ctx.
func principal(ctx context.Context) (auth.Principal, error) {
	p, ok := auth.PrincipalFrom(ctx)
	if !ok {
		return auth.Principal{}, ErrUnauthenticated("missing credential")
	}
	return p, nil
}

// RequireUser returns the principal of an RPC without organization_id
// (CreateOrganization, ListOrganizations, AcceptInvitation), which accepts
// only Logto users.
func (s *Service) RequireUser(ctx context.Context) (auth.Principal, error) {
	p, err := principal(ctx)
	if err != nil {
		return p, err
	}
	if !p.IsUser() {
		return p, ErrPermission("only signed-in users can call this RPC")
	}
	return p, nil
}

// Authorize applies the authorization matrix for an RPC that takes an
// organization_id. It loads the caller's current membership through q (the
// pool, or the transaction of the RPC):
//
//   - A user without a membership gets not_found.
//   - A personal API key of another organization gets not_found. A personal
//     API key acts as its user with the user's current role, and only for
//     the actions that authz.APIKeyAllowed lists; other actions get
//     permission_denied.
//   - A role that the matrix does not allow gets permission_denied.
//
// Later layers must load every target entity with the organization in the
// query (WHERE organization_id = caller.OrganizationID AND ...) and answer
// not_found when it has no row, so an entity of another organization is
// never visible.
func (s *Service) Authorize(ctx context.Context, q db.Querier, orgID string, action authz.Action) (Caller, error) {
	p, err := principal(ctx)
	if err != nil {
		return Caller{}, err
	}
	orgID, err = NormalizeID("organization_id", orgID)
	if err != nil {
		return Caller{}, err
	}
	SetRPCOrganization(ctx, orgID)
	c := Caller{Principal: p, UserID: p.UserID, OrganizationID: orgID}
	switch p.Kind {
	case auth.PrincipalUser:
	case auth.PrincipalAPIKey:
		if p.OrganizationID != orgID {
			return c, ErrNotFound("organization")
		}
	default:
		return c, ErrPermission("this credential is not accepted here")
	}
	var role int16
	err = q.QueryRow(ctx, "SELECT membership_id::text, role FROM memberships WHERE organization_id = $1 AND user_id = $2", orgID, p.UserID).
		Scan(&c.MembershipID, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, ErrNotFound("organization")
	}
	if err != nil {
		return c, s.Internal(ctx, "load membership", err)
	}
	c.Role = tidev1.Role(role)
	if p.IsAPIKey() && !authz.APIKeyAllowed(action) {
		return c, ErrPermission("a personal API key cannot perform this action")
	}
	if !authz.RoleAllowed(c.Role, action) {
		return c, ErrPermission("the caller's role does not allow this action")
	}
	return c, nil
}

// authorizeOwnerLocked authorizes an owner-only RPC that takes the
// organization lock: DeleteOrganization and TransferOwnership. The first
// Authorize is only a pre-check, so callers without the owner role fail
// without queueing on the lock; the authoritative check runs again under the
// lock, so a transfer that committed first is visible to a deletion and the
// other way round.
func (s *Service) authorizeOwnerLocked(ctx context.Context, tx pgx.Tx, orgID string) (Caller, error) {
	pre, err := s.Authorize(ctx, tx, orgID, authz.ActionOwnerOnly)
	if err != nil {
		return Caller{}, err
	}
	return s.authorizeOwnerAfterLock(ctx, tx, pre.OrganizationID)
}

// authorizeOwnerAfterLock is for callers that already ran the owner
// pre-check and have the canonical organization ID.
func (s *Service) authorizeOwnerAfterLock(ctx context.Context, tx pgx.Tx, orgID string) (Caller, error) {
	if err := LockOrganization(ctx, tx, orgID); err != nil {
		return Caller{}, err
	}
	return s.Authorize(ctx, tx, orgID, authz.ActionOwnerOnly)
}

// LockOrganization takes the row lock of the organization until the
// transaction ends. An organization that a concurrent deletion removed while
// the caller waited yields not_found. orgID must be canonical.
func LockOrganization(ctx context.Context, tx pgx.Tx, orgID string) error {
	var locked int
	err := tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR UPDATE", orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound("organization")
	}
	return err
}

// LockOrganizationShare takes FOR KEY SHARE on the organization row until
// the transaction ends. DeleteOrganization takes the row FOR UPDATE first and
// then changes and deletes the rows of the organization. A transaction that
// locks or changes rows of an organization calls this helper first, so it
// takes the locks in the same order as the deletion and cannot deadlock with
// it; such transactions share the lock with each other. An organization that
// a concurrent deletion removed yields not_found. orgID must be canonical.
func LockOrganizationShare(ctx context.Context, tx pgx.Tx, orgID string) error {
	var locked int
	err := tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR KEY SHARE", orgID).Scan(&locked)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound("organization")
	}
	return err
}

// AllowOwnSandLinkValidation takes a token of the rate limit of the OwnSand
// link validation in SetOwnSandLink: 10 calls per user per minute with a
// burst of 10, and the shared per-client bucket. The OwnSand link layer
// calls it right before it calls OwnSand ListDevices.
func (s *Service) AllowOwnSandLinkValidation(ctx context.Context, userID string) error {
	if ok, retry := s.Limits.PerUser.Allow("ownsand-link:"+userID, 1); !ok {
		return ErrRateLimited(retry)
	}
	if addr := ClientAddr(ctx); addr != "" {
		if ok, retry := s.Limits.PerClient.Allow(addr, 1); !ok {
			return ErrRateLimited(retry)
		}
	}
	return nil
}
