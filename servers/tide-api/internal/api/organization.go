// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/auth"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
)

// OrganizationHandler serves OrganizationService.
type OrganizationHandler struct{ *Service }

var _ tidev1connect.OrganizationServiceHandler = OrganizationHandler{}

// settingsPathMaxDevSessions is the only field mask path of
// UpdateOrganizationSettings.
const settingsPathMaxDevSessions = "max_dev_sessions_per_user"

func organizationProto(id, name string, created time.Time, role tidev1.Role, maxDevSessions *int32) *tidev1.Organization {
	return &tidev1.Organization{
		OrganizationId: id, Name: name, CreateTime: Timestamp(created), CallerRole: role,
		Settings: &tidev1.OrganizationSettings{MaxDevSessionsPerUser: maxDevSessions},
	}
}

func (h OrganizationHandler) CreateOrganization(ctx context.Context, req *connect.Request[tidev1.CreateOrganizationRequest]) (*connect.Response[tidev1.CreateOrganizationResponse], error) {
	p, err := h.RequireUser(ctx)
	if err != nil {
		return nil, err
	}
	name := req.Msg.GetName()
	if err := ValidateName("name", name, maxOrganizationNameLength); err != nil {
		return nil, err
	}
	orgID := NewID()
	now := h.NowUTC()
	SetRPCOrganization(ctx, orgID)
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "INSERT INTO organizations (organization_id, name, create_time) VALUES ($1, $2, $3)", orgID, name, now); err != nil {
			return err
		}
		membershipID := NewID()
		if _, err := tx.Exec(ctx, "INSERT INTO memberships (membership_id, organization_id, user_id, role, join_time) VALUES ($1, $2, $3, $4, $5)",
			membershipID, orgID, p.UserID, int16(tidev1.Role_ROLE_OWNER), now); err != nil {
			return err
		}
		return h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: orgID, Action: tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_CREATED, Actor: p,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION, TargetID: orgID,
		})
	})
	if err != nil {
		return nil, h.Internal(ctx, "create organization", err)
	}
	return connect.NewResponse(&tidev1.CreateOrganizationResponse{
		Organization: organizationProto(orgID, name, now, tidev1.Role_ROLE_OWNER, nil),
	}), nil
}

// loadOrganization reads one organization for the caller.
func (s *Service) loadOrganization(ctx context.Context, c Caller) (*tidev1.Organization, error) {
	var name string
	var created time.Time
	var maxDev *int32
	err := s.DB.Pool.QueryRow(ctx, "SELECT name, create_time, max_dev_sessions_per_user FROM organizations WHERE organization_id = $1", c.OrganizationID).
		Scan(&name, &created, &maxDev)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound("organization")
	}
	if err != nil {
		return nil, s.Internal(ctx, "get organization", err)
	}
	return organizationProto(c.OrganizationID, name, created, c.Role, maxDev), nil
}

func (h OrganizationHandler) GetOrganization(ctx context.Context, req *connect.Request[tidev1.GetOrganizationRequest]) (*connect.Response[tidev1.GetOrganizationResponse], error) {
	c, err := h.Authorize(ctx, h.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	org, err := h.loadOrganization(ctx, c)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&tidev1.GetOrganizationResponse{Organization: org}), nil
}

func (h OrganizationHandler) ListOrganizations(ctx context.Context, req *connect.Request[tidev1.ListOrganizationsRequest]) (*connect.Response[tidev1.ListOrganizationsResponse], error) {
	p, err := h.RequireUser(ctx)
	if err != nil {
		return nil, err
	}
	limit, after, err := PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT o.organization_id::text, o.name, o.create_time, o.max_dev_sessions_per_user, m.role
		FROM memberships m JOIN organizations o ON o.organization_id = m.organization_id
		WHERE m.user_id = $1 AND o.organization_id > $2
		ORDER BY o.organization_id LIMIT $3`, p.UserID, AfterKey(after), limit)
	if err != nil {
		return nil, h.Internal(ctx, "list organizations", err)
	}
	defer rows.Close()
	resp := &tidev1.ListOrganizationsResponse{}
	last := ""
	for rows.Next() {
		var id, name string
		var created time.Time
		var maxDev *int32
		var role int16
		if err := rows.Scan(&id, &name, &created, &maxDev, &role); err != nil {
			return nil, h.Internal(ctx, "list organizations", err)
		}
		resp.Organizations = append(resp.Organizations, organizationProto(id, name, created, tidev1.Role(role), maxDev))
		last = id
	}
	if err := rows.Err(); err != nil {
		return nil, h.Internal(ctx, "list organizations", err)
	}
	resp.NextPageToken = NextPageToken(len(resp.Organizations), limit, last)
	return connect.NewResponse(resp), nil
}

// DeleteOrganization runs the organization deletion hooks of later layers
// (Prepare outside the transaction, InTx inside it), then revokes every
// personal API key of the organization, records the audit events and
// deletes the organization row, which deletes the memberships, invitations,
// API keys and every row of later layers that references it. Audit events
// stay for their retention.
func (h OrganizationHandler) DeleteOrganization(ctx context.Context, req *connect.Request[tidev1.DeleteOrganizationRequest]) (*connect.Response[tidev1.DeleteOrganizationResponse], error) {
	pre, err := h.Authorize(ctx, h.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionOwnerOnly)
	if err != nil {
		return nil, err
	}
	d := OrganizationDeletion{OrganizationID: pre.OrganizationID, Actor: pre.Principal}
	if err := h.organizationDeletionPrepare(ctx, d); err != nil {
		return nil, h.Internal(ctx, "prepare organization deletion", err)
	}
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.authorizeOwnerLocked(ctx, tx, d.OrganizationID)
		if err != nil {
			return err
		}
		if err := h.organizationDeletionInTx(ctx, tx, d); err != nil {
			return err
		}
		if _, err := h.revokeAPIKeysTx(ctx, tx, c.OrganizationID, "", c.Principal); err != nil {
			return err
		}
		if err := h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION, TargetID: c.OrganizationID,
		}); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, "DELETE FROM organizations WHERE organization_id = $1", c.OrganizationID)
		return err
	})
	if err != nil {
		return nil, h.Internal(ctx, "delete organization", err)
	}
	h.organizationDeletionAfterCommit(ctx, d)
	return connect.NewResponse(&tidev1.DeleteOrganizationResponse{}), nil
}

// revokeAPIKeysTx revokes the active personal API keys of the organization,
// of one user when userID is not empty, and records an API_KEY_REVOKED audit
// event for each. It returns the revoked key IDs.
func (s *Service) revokeAPIKeysTx(ctx context.Context, tx pgx.Tx, orgID, userID string, actor auth.Principal) ([]string, error) {
	rows, err := tx.Query(ctx, `UPDATE api_keys SET revoke_time = $3
		WHERE organization_id = $1 AND ($2 = '' OR user_id = $2) AND revoke_time IS NULL
		RETURNING api_key_id::text, user_id`, orgID, userID, s.NowUTC())
	if err != nil {
		return nil, err
	}
	type revoked struct{ id, user string }
	var keys []revoked
	for rows.Next() {
		var k revoked
		if err := rows.Scan(&k.id, &k.user); err != nil {
			rows.Close()
			return nil, err
		}
		keys = append(keys, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(keys))
	for _, k := range keys {
		if err := s.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: orgID, Action: tidev1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED, Actor: actor,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: k.id, SubjectUserID: k.user,
		}); err != nil {
			return nil, err
		}
		ids = append(ids, k.id)
	}
	return ids, nil
}

func (h OrganizationHandler) TransferOwnership(ctx context.Context, req *connect.Request[tidev1.TransferOwnershipRequest]) (*connect.Response[tidev1.TransferOwnershipResponse], error) {
	target := req.Msg.GetUserId()
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.authorizeOwnerLocked(ctx, tx, req.Msg.GetOrganizationId())
		if err != nil {
			return err
		}
		if target == "" {
			return ErrInvalidf("invalid user_id: is required")
		}
		if target == c.UserID {
			return ErrInvalidf("invalid user_id: the caller is already the owner")
		}
		membershipID, _, err := loadMembershipForUpdate(ctx, tx, c.OrganizationID, target)
		if err != nil {
			return err
		}
		// The old owner becomes admin first, so the one-owner index holds
		// after each statement.
		if _, err := tx.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", c.OrganizationID, c.UserID, int16(tidev1.Role_ROLE_ADMIN)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2", c.OrganizationID, target, int16(tidev1.Role_ROLE_OWNER)); err != nil {
			return err
		}
		return h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_OWNERSHIP_TRANSFERRED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID, SubjectUserID: target,
		})
	})
	if err != nil {
		return nil, h.Internal(ctx, "transfer ownership", err)
	}
	return connect.NewResponse(&tidev1.TransferOwnershipResponse{}), nil
}

func (h OrganizationHandler) UpdateOrganizationSettings(ctx context.Context, req *connect.Request[tidev1.UpdateOrganizationSettingsRequest]) (*connect.Response[tidev1.UpdateOrganizationSettingsResponse], error) {
	paths := req.Msg.GetUpdateMask().GetPaths()
	var resp *tidev1.UpdateOrganizationSettingsResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageSettings)
		if err != nil {
			return err
		}
		if len(paths) == 0 {
			return ErrInvalidf("invalid update_mask: must name at least one path")
		}
		setMax := false
		for _, p := range paths {
			if p != settingsPathMaxDevSessions {
				return ErrInvalidf("invalid update_mask: unknown path %q", p)
			}
			setMax = true
		}
		// An omitted settings message has no value for the masked path, so
		// it clears the setting like an empty message does.
		var newMax *int32
		if settings := req.Msg.GetSettings(); setMax && settings != nil && settings.MaxDevSessionsPerUser != nil {
			v := settings.GetMaxDevSessionsPerUser()
			if v <= 0 {
				return ErrInvalidf("invalid settings.max_dev_sessions_per_user: must be greater than zero, or unset for no limit")
			}
			newMax = &v
		}
		var name string
		var created time.Time
		var oldMax *int32
		err = tx.QueryRow(ctx, "SELECT name, create_time, max_dev_sessions_per_user FROM organizations WHERE organization_id = $1 FOR UPDATE", c.OrganizationID).
			Scan(&name, &created, &oldMax)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound("organization")
		}
		if err != nil {
			return err
		}
		// Lowering the limit ends no existing dev session; the dev session
		// layer checks it only in CreateDevSession.
		if !equalOptional(oldMax, newMax) {
			if _, err := tx.Exec(ctx, "UPDATE organizations SET max_dev_sessions_per_user = $2 WHERE organization_id = $1", c.OrganizationID, newMax); err != nil {
				return err
			}
			if err := h.WriteAudit(ctx, tx, AuditEvent{
				OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_ORGANIZATION_SETTINGS_CHANGED, Actor: c.Principal,
				TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION, TargetID: c.OrganizationID,
			}); err != nil {
				return err
			}
		}
		resp = &tidev1.UpdateOrganizationSettingsResponse{Organization: organizationProto(c.OrganizationID, name, created, c.Role, newMax)}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "update organization settings", err)
	}
	return connect.NewResponse(resp), nil
}

func equalOptional(a, b *int32) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}
