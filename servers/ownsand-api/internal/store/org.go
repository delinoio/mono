// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ErrForbidden is returned when the caller's role does not allow a change of
// this particular target (for example, an admin that changes the owner).
var ErrForbidden = errors.New("forbidden")

// Organization is an organization row with the caller's role.
type Organization struct {
	ID         string
	Name       string
	CreatedAt  time.Time
	CallerRole ownsandv1.Role
}

// Membership is a membership row.
type Membership struct {
	ID             string
	OrganizationID string
	UserID         string
	Role           ownsandv1.Role
	CreatedAt      time.Time
}

// Invitation is an invitation row.
type Invitation struct {
	ID             string
	OrganizationID string
	Role           ownsandv1.Role
	SecretPrefix   string
	CreatedBy      Actor
	CreatedAt      time.Time
	ExpiresAt      time.Time
	AcceptedAt     *time.Time
	RevokedAt      *time.Time
}

// State derives the invitation state at now.
func (i Invitation) State(now time.Time) ownsandv1.InvitationState {
	switch {
	case i.AcceptedAt != nil:
		return ownsandv1.InvitationState_INVITATION_STATE_ACCEPTED
	case i.RevokedAt != nil:
		return ownsandv1.InvitationState_INVITATION_STATE_REVOKED
	case !now.Before(i.ExpiresAt):
		return ownsandv1.InvitationState_INVITATION_STATE_EXPIRED
	default:
		return ownsandv1.InvitationState_INVITATION_STATE_PENDING
	}
}

// CreateOrganization creates the organization, the caller's owner membership
// and the default agent version policy in one transaction.
func (s *Store) CreateOrganization(ctx context.Context, name, userID, requestID string) (Organization, error) {
	now := s.now()
	org := Organization{ID: NewID(), Name: name, CreatedAt: now, CallerRole: ownsandv1.Role_ROLE_OWNER}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO organizations (organization_id, name, created_at) VALUES ($1, $2, $3)`, org.ID, name, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO memberships (membership_id, organization_id, user_id, role, created_at) VALUES ($1, $2, $3, $4, $5)`,
			NewID(), org.ID, userID, int16(ownsandv1.Role_ROLE_OWNER), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_version_policies (organization_id, mode, updated_at) VALUES ($1, $2, $3)`,
			org.ID, int16(ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT), now); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: org.ID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED,
			Actor: Actor{ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, userID}, TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION,
			TargetID: org.ID, RequestID: requestID,
		})
	})
	return org, err
}

// GetMembership returns the user's membership in a live organization, or
// ErrNotFound.
func (s *Store) GetMembership(ctx context.Context, organizationID, userID string) (Membership, error) {
	if !ValidID(organizationID) {
		return Membership{}, ErrNotFound
	}
	var m Membership
	var role int16
	err := s.Pool.QueryRow(ctx, `SELECT m.membership_id, m.organization_id, m.user_id, m.role, m.created_at
		FROM memberships m JOIN organizations o USING (organization_id)
		WHERE m.organization_id = $1 AND m.user_id = $2 AND o.deleted_at IS NULL`, organizationID, userID).
		Scan(&m.ID, &m.OrganizationID, &m.UserID, &role, &m.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Membership{}, ErrNotFound
	}
	m.Role = ownsandv1.Role(role)
	return m, err
}

// OrganizationLive reports whether the organization exists and is not deleted.
func (s *Store) OrganizationLive(ctx context.Context, organizationID string) (bool, error) {
	if !ValidID(organizationID) {
		return false, nil
	}
	var ok bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE organization_id = $1 AND deleted_at IS NULL)`, organizationID).Scan(&ok)
	return ok, err
}

// GetOrganization returns a live organization.
func (s *Store) GetOrganization(ctx context.Context, organizationID string) (Organization, error) {
	var o Organization
	err := s.Pool.QueryRow(ctx, `SELECT organization_id, name, created_at FROM organizations WHERE organization_id = $1 AND deleted_at IS NULL`, organizationID).
		Scan(&o.ID, &o.Name, &o.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Organization{}, ErrNotFound
	}
	return o, err
}

// ListOrganizations lists the live organizations in which the user has a
// membership.
func (s *Store) ListOrganizations(ctx context.Context, userID string, p Page) ([]Organization, error) {
	rows, err := s.Pool.Query(ctx, `SELECT o.organization_id, o.name, o.created_at, m.role
		FROM memberships m JOIN organizations o USING (organization_id)
		WHERE m.user_id = $1 AND o.deleted_at IS NULL AND o.organization_id > $2
		ORDER BY o.organization_id LIMIT $3`, userID, afterArg(p), p.Size)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Organization, error) {
		var o Organization
		var role int16
		err := r.Scan(&o.ID, &o.Name, &o.CreatedAt, &role)
		o.CallerRole = ownsandv1.Role(role)
		return o, err
	})
}

// TransferOwnership makes another existing member the owner; the previous
// owner becomes admin.
func (s *Store) TransferOwnership(ctx context.Context, organizationID string, actor Actor, newOwnerUserID, requestID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var targetID string
		var role int16
		err := tx.QueryRow(ctx, `SELECT membership_id, role FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE`,
			organizationID, newOwnerUserID).Scan(&targetID, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.Role(role) == ownsandv1.Role_ROLE_OWNER {
			// Transferring to the current owner changes nothing.
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2 AND role = $4`,
			organizationID, actor.ID, int16(ownsandv1.Role_ROLE_ADMIN), int16(ownsandv1.Role_ROLE_OWNER)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE memberships SET role = $2 WHERE membership_id = $1`, targetID, int16(ownsandv1.Role_ROLE_OWNER)); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_OWNERSHIP_TRANSFERRED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: targetID, RequestID: requestID,
		})
	})
}

// CreateInvitation stores an invitation with the hash of its secret.
func (s *Store) CreateInvitation(ctx context.Context, organizationID string, role ownsandv1.Role, actor Actor, secretHash []byte, secretPrefix string, ttl time.Duration, requestID string) (Invitation, error) {
	now := s.now()
	inv := Invitation{ID: NewID(), OrganizationID: organizationID, Role: role, SecretPrefix: secretPrefix, CreatedBy: actor, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO invitations (invitation_id, organization_id, role, secret_hash, secret_prefix, created_by_kind, created_by_id, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			inv.ID, organizationID, int16(role), secretHash, secretPrefix, int16(actor.Kind), actor.ID, now, inv.ExpiresAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_CREATED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION, TargetID: inv.ID, RequestID: requestID,
		})
	})
	return inv, err
}

const invitationColumns = `invitation_id, organization_id, role, secret_prefix, created_by_kind, created_by_id, created_at, expires_at, accepted_at, revoked_at`

func scanInvitation(r pgx.Row) (Invitation, error) {
	var inv Invitation
	var role, kind int16
	err := r.Scan(&inv.ID, &inv.OrganizationID, &role, &inv.SecretPrefix, &kind, &inv.CreatedBy.ID, &inv.CreatedAt, &inv.ExpiresAt, &inv.AcceptedAt, &inv.RevokedAt)
	inv.Role = ownsandv1.Role(role)
	inv.CreatedBy.Kind = ownsandv1.PrincipalKind(kind)
	return inv, err
}

// ListInvitations lists the pending invitations of the organization. Accepted,
// revoked and expired invitations are not listed.
func (s *Store) ListInvitations(ctx context.Context, organizationID string, p Page) ([]Invitation, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+invitationColumns+` FROM invitations
		WHERE organization_id = $1 AND invitation_id > $2 AND accepted_at IS NULL AND revoked_at IS NULL AND expires_at > $4
		ORDER BY invitation_id LIMIT $3`, organizationID, afterArg(p), p.Size, s.now())
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Invitation, error) { return scanInvitation(r) })
}

// RevokeInvitation revokes a pending invitation. Revoking an already revoked
// invitation succeeds without change; an accepted or expired one fails with
// ErrConflict.
func (s *Store) RevokeInvitation(ctx context.Context, organizationID, invitationID string, actor Actor, requestID string) error {
	if !ValidID(invitationID) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		inv, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE organization_id = $1 AND invitation_id = $2 FOR UPDATE`, organizationID, invitationID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch inv.State(s.now()) {
		case ownsandv1.InvitationState_INVITATION_STATE_REVOKED:
			return nil
		case ownsandv1.InvitationState_INVITATION_STATE_PENDING:
		default:
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE invitations SET revoked_at = $2 WHERE invitation_id = $1`, invitationID, s.now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION, TargetID: invitationID, RequestID: requestID,
		})
	})
}

// AcceptInvitation creates the membership of the user from a pending
// invitation. An unknown, expired, revoked or used invitation, or one of a
// deleted organization, fails with ErrNotFound; an existing membership fails
// with ErrAlreadyExists and leaves the invitation unused.
func (s *Store) AcceptInvitation(ctx context.Context, secretHash []byte, userID, requestID string) (Organization, Membership, error) {
	var org Organization
	var m Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		inv, err := scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM invitations WHERE secret_hash = $1 FOR UPDATE`, secretHash))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if inv.State(s.now()) != ownsandv1.InvitationState_INVITATION_STATE_PENDING {
			return ErrNotFound
		}
		err = tx.QueryRow(ctx, `SELECT organization_id, name, created_at FROM organizations WHERE organization_id = $1 AND deleted_at IS NULL FOR SHARE`, inv.OrganizationID).
			Scan(&org.ID, &org.Name, &org.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		m = Membership{ID: NewID(), OrganizationID: inv.OrganizationID, UserID: userID, Role: inv.Role, CreatedAt: s.now()}
		tag, err := tx.Exec(ctx, `INSERT INTO memberships (membership_id, organization_id, user_id, role, created_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (organization_id, user_id) DO NOTHING`, m.ID, m.OrganizationID, userID, int16(m.Role), m.CreatedAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyExists
		}
		org.CallerRole = m.Role
		if _, err := tx.Exec(ctx, `UPDATE invitations SET accepted_at = $2, accepted_by = $3 WHERE invitation_id = $1`, inv.ID, s.now(), userID); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: inv.OrganizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED,
			Actor: Actor{ownsandv1.PrincipalKind_PRINCIPAL_KIND_USER, userID}, TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP,
			TargetID: m.ID, RequestID: requestID,
		})
	})
	return org, m, err
}

// ListMembers lists the memberships of the organization.
func (s *Store) ListMembers(ctx context.Context, organizationID string, p Page) ([]Membership, error) {
	rows, err := s.Pool.Query(ctx, `SELECT membership_id, organization_id, user_id, role, created_at FROM memberships
		WHERE organization_id = $1 AND membership_id > $2 ORDER BY membership_id LIMIT $3`, organizationID, afterArg(p), p.Size)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Membership, error) {
		var m Membership
		var role int16
		err := r.Scan(&m.ID, &m.OrganizationID, &m.UserID, &role, &m.CreatedAt)
		m.Role = ownsandv1.Role(role)
		return m, err
	})
}

// UpdateMemberRole changes a member's role between admin and member. The
// owner's role cannot be changed this way (ErrForbidden).
func (s *Store) UpdateMemberRole(ctx context.Context, organizationID, userID string, role ownsandv1.Role, actor Actor, requestID string) (Membership, error) {
	var m Membership
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var current int16
		err := tx.QueryRow(ctx, `SELECT membership_id, organization_id, user_id, role, created_at FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE`,
			organizationID, userID).Scan(&m.ID, &m.OrganizationID, &m.UserID, &current, &m.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.Role(current) == ownsandv1.Role_ROLE_OWNER {
			return ErrForbidden
		}
		m.Role = role
		if ownsandv1.Role(current) == role {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE memberships SET role = $2 WHERE membership_id = $1`, m.ID, int16(role)); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_ROLE_CHANGED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: m.ID, RequestID: requestID,
		})
	})
	return m, err
}

// RemoveMember deletes a membership other than the owner's (ErrForbidden)
// and writes the revocation event of the user's sessions and relay grants in
// the organization.
func (s *Store) RemoveMember(ctx context.Context, organizationID, userID string, actor Actor, requestID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var id string
		var role int16
		err := tx.QueryRow(ctx, `SELECT membership_id, role FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE`, organizationID, userID).Scan(&id, &role)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.Role(role) == ownsandv1.Role_ROLE_OWNER {
			return ErrForbidden
		}
		if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE membership_id = $1`, id); err != nil {
			return err
		}
		if err := s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventMembershipEnded, OrganizationID: organizationID, UserID: userID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: id, RequestID: requestID,
		})
	})
}
