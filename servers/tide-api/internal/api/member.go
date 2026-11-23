// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
	"github.com/delinoio/mono/protos/delino/tide/v1/tidev1connect"
	"github.com/delinoio/mono/servers/tide-api/internal/auth"
	"github.com/delinoio/mono/servers/tide-api/internal/authz"
	"github.com/delinoio/mono/servers/tide-api/internal/db"
)

// MemberHandler serves MemberService.
type MemberHandler struct{ *Service }

var _ tidev1connect.MemberServiceHandler = MemberHandler{}

const invitationColumns = `invitation_id::text, organization_id::text, role, secret_prefix, created_by_user_id,
	create_time, expire_time, accept_time, revoke_time`

func (s *Service) scanInvitation(row pgx.Row) (*tidev1.Invitation, error) {
	var inv tidev1.Invitation
	var role int16
	var created, expires time.Time
	var accepted, revoked *time.Time
	if err := row.Scan(&inv.InvitationId, &inv.OrganizationId, &role, &inv.SecretPrefix, &inv.CreatedByUserId, &created, &expires, &accepted, &revoked); err != nil {
		return nil, err
	}
	inv.Role = tidev1.Role(role)
	inv.CreateTime = Timestamp(created)
	inv.ExpireTime = Timestamp(expires)
	switch {
	case accepted != nil:
		inv.State = tidev1.InvitationState_INVITATION_STATE_ACCEPTED
	case revoked != nil:
		inv.State = tidev1.InvitationState_INVITATION_STATE_REVOKED
	case !s.NowUTC().Before(expires):
		inv.State = tidev1.InvitationState_INVITATION_STATE_EXPIRED
	default:
		inv.State = tidev1.InvitationState_INVITATION_STATE_PENDING
	}
	return &inv, nil
}

func (h MemberHandler) CreateInvitation(ctx context.Context, req *connect.Request[tidev1.CreateInvitationRequest]) (*connect.Response[tidev1.CreateInvitationResponse], error) {
	role := req.Msg.GetRole()
	var resp *tidev1.CreateInvitationResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
		if err != nil {
			return err
		}
		if role != tidev1.Role_ROLE_ADMIN && role != tidev1.Role_ROLE_MEMBER {
			return ErrInvalidf("invalid role: must be ROLE_ADMIN or ROLE_MEMBER")
		}
		if err := LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		secret, err := auth.NewSecret(auth.SecretInvitation)
		if err != nil {
			return err
		}
		id := NewID()
		now := h.NowUTC()
		inv, err := h.scanInvitation(tx.QueryRow(ctx, `INSERT INTO invitations (invitation_id, organization_id, role, secret_sha256, secret_prefix, created_by_user_id, create_time, expire_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+invitationColumns,
			id, c.OrganizationID, int16(role), secret.Hash, secret.DisplayPrefix, c.UserID, now, now.Add(InvitationLifetime)))
		if err != nil {
			return err
		}
		if err := h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_INVITATION_CREATED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION, TargetID: id,
		}); err != nil {
			return err
		}
		resp = &tidev1.CreateInvitationResponse{Invitation: inv, InvitationSecret: secret.Value}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "create invitation", err)
	}
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) ListInvitations(ctx context.Context, req *connect.Request[tidev1.ListInvitationsRequest]) (*connect.Response[tidev1.ListInvitationsResponse], error) {
	c, err := h.Authorize(ctx, h.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
	if err != nil {
		return nil, err
	}
	limit, after, err := PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT `+invitationColumns+` FROM invitations
		WHERE organization_id = $1 AND invitation_id > $2 AND accept_time IS NULL AND revoke_time IS NULL AND expire_time > $3
		ORDER BY invitation_id LIMIT $4`, c.OrganizationID, AfterKey(after), h.NowUTC(), limit)
	if err != nil {
		return nil, h.Internal(ctx, "list invitations", err)
	}
	defer rows.Close()
	resp := &tidev1.ListInvitationsResponse{}
	for rows.Next() {
		inv, err := h.scanInvitation(rows)
		if err != nil {
			return nil, h.Internal(ctx, "list invitations", err)
		}
		resp.Invitations = append(resp.Invitations, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, h.Internal(ctx, "list invitations", err)
	}
	last := ""
	if n := len(resp.Invitations); n > 0 {
		last = resp.Invitations[n-1].GetInvitationId()
	}
	resp.NextPageToken = NextPageToken(len(resp.Invitations), limit, last)
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) RevokeInvitation(ctx context.Context, req *connect.Request[tidev1.RevokeInvitationRequest]) (*connect.Response[tidev1.RevokeInvitationResponse], error) {
	var resp *tidev1.RevokeInvitationResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
		if err != nil {
			return err
		}
		invID, err := NormalizeID("invitation_id", req.Msg.GetInvitationId())
		if err != nil {
			return err
		}
		if err := LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		inv, err := h.scanInvitation(tx.QueryRow(ctx, "SELECT "+invitationColumns+" FROM invitations WHERE organization_id = $1 AND invitation_id = $2 FOR UPDATE", c.OrganizationID, invID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound("invitation")
		}
		if err != nil {
			return err
		}
		if inv.GetState() != tidev1.InvitationState_INVITATION_STATE_PENDING {
			return ErrPrecondition("only a pending invitation can be revoked")
		}
		inv, err = h.scanInvitation(tx.QueryRow(ctx, "UPDATE invitations SET revoke_time = $3 WHERE organization_id = $1 AND invitation_id = $2 RETURNING "+invitationColumns, c.OrganizationID, invID, h.NowUTC()))
		if err != nil {
			return err
		}
		if err := h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION, TargetID: invID,
		}); err != nil {
			return err
		}
		resp = &tidev1.RevokeInvitationResponse{Invitation: inv}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "revoke invitation", err)
	}
	return connect.NewResponse(resp), nil
}

// errInvitationInvalid is the one answer for an unknown, expired, revoked
// or used invitation, so the response does not show which condition failed.
func errInvitationInvalid() error { return ErrNotFound("invitation") }

// AcceptInvitation applies the rate limits of the contract before it looks
// at the secret: 10 calls per invitation display prefix and 10 per user per
// minute, and the shared per-client bucket.
func (h MemberHandler) AcceptInvitation(ctx context.Context, req *connect.Request[tidev1.AcceptInvitationRequest]) (*connect.Response[tidev1.AcceptInvitationResponse], error) {
	p, err := h.RequireUser(ctx)
	if err != nil {
		return nil, err
	}
	secret := req.Msg.GetInvitationSecret()
	if ok, retry := h.Limits.PerPrefix.Allow("invitation:"+auth.DisplayPrefix(auth.SecretInvitation, secret), 1); !ok {
		return nil, ErrRateLimited(retry)
	}
	if ok, retry := h.Limits.PerUser.Allow("accept-invitation:"+p.UserID, 1); !ok {
		return nil, ErrRateLimited(retry)
	}
	if addr := ClientAddr(ctx); addr != "" {
		if ok, retry := h.Limits.PerClient.Allow(addr, 1); !ok {
			return nil, ErrRateLimited(retry)
		}
	}
	if !auth.WellFormed(auth.SecretInvitation, secret) {
		return nil, errInvitationInvalid()
	}
	hash := auth.HashSecret(secret)
	var resp *tidev1.AcceptInvitationResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		// Lock order: organization row first, invitation row second, the
		// order of DeleteOrganization. The first read takes no lock and only
		// names the organization; every condition is checked again under
		// both locks.
		now := h.NowUTC()
		var preOrgID string
		err := tx.QueryRow(ctx, `SELECT organization_id::text FROM invitations
			WHERE secret_sha256 = $1 AND accept_time IS NULL AND revoke_time IS NULL AND expire_time > $2`, hash, now).Scan(&preOrgID)
		if errors.Is(err, pgx.ErrNoRows) {
			return errInvitationInvalid()
		}
		if err != nil {
			return err
		}
		if err := LockOrganizationShare(ctx, tx, preOrgID); err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				// A deletion removed the organization and its invitations.
				return errInvitationInvalid()
			}
			return err
		}
		var invID, orgID, orgName string
		var role int16
		var stored []byte
		var orgCreated time.Time
		var maxDev *int32
		err = tx.QueryRow(ctx, `SELECT i.invitation_id::text, i.organization_id::text, i.role, i.secret_sha256, o.name, o.create_time, o.max_dev_sessions_per_user
			FROM invitations i JOIN organizations o ON o.organization_id = i.organization_id
			WHERE i.secret_sha256 = $1 AND i.accept_time IS NULL AND i.revoke_time IS NULL AND i.expire_time > $2
			FOR UPDATE OF i`, hash, now).Scan(&invID, &orgID, &role, &stored, &orgName, &orgCreated, &maxDev)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.HashEqual(stored, hash)) {
			return errInvitationInvalid()
		}
		if err != nil {
			return err
		}
		SetRPCOrganization(ctx, orgID)
		membershipID := NewID()
		// ON CONFLICT turns a second membership of the same user (also from
		// a concurrent acceptance) into zero affected rows.
		tag, err := tx.Exec(ctx, `INSERT INTO memberships (membership_id, organization_id, user_id, role, join_time) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (organization_id, user_id) DO NOTHING`, membershipID, orgID, p.UserID, role, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrAlreadyExists("the caller is already a member of the organization")
		}
		if _, err := tx.Exec(ctx, "UPDATE invitations SET accept_time = $2, accepted_by_user_id = $3 WHERE invitation_id = $1", invID, now, p.UserID); err != nil {
			return err
		}
		if err := h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: orgID, Action: tidev1.AuditAction_AUDIT_ACTION_MEMBER_JOINED, Actor: p,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID, SubjectUserID: p.UserID,
		}); err != nil {
			return err
		}
		resp = &tidev1.AcceptInvitationResponse{
			Organization: organizationProto(orgID, orgName, orgCreated, tidev1.Role(role), maxDev),
			Member:       &tidev1.Member{OrganizationId: orgID, UserId: p.UserID, Role: tidev1.Role(role), JoinTime: Timestamp(now)},
		}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "accept invitation", err)
	}
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) ListMembers(ctx context.Context, req *connect.Request[tidev1.ListMembersRequest]) (*connect.Response[tidev1.ListMembersResponse], error) {
	c, err := h.Authorize(ctx, h.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, after, err := PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT membership_id::text, user_id, role, join_time FROM memberships
		WHERE organization_id = $1 AND membership_id > $2 ORDER BY membership_id LIMIT $3`, c.OrganizationID, AfterKey(after), limit)
	if err != nil {
		return nil, h.Internal(ctx, "list members", err)
	}
	defer rows.Close()
	resp := &tidev1.ListMembersResponse{}
	last := ""
	var userIDs []string
	for rows.Next() {
		m := &tidev1.Member{OrganizationId: c.OrganizationID}
		var role int16
		var joined time.Time
		if err := rows.Scan(&last, &m.UserId, &role, &joined); err != nil {
			return nil, h.Internal(ctx, "list members", err)
		}
		m.Role = tidev1.Role(role)
		m.JoinTime = Timestamp(joined)
		resp.Members = append(resp.Members, m)
		userIDs = append(userIDs, m.UserId)
	}
	if err := rows.Err(); err != nil {
		return nil, h.Internal(ctx, "list members", err)
	}
	// Display names come from delino-accounts only for the members of this
	// page; when the API is unavailable the members are returned without
	// them.
	if h.DisplayNames != nil && len(userIDs) > 0 {
		names, err := h.DisplayNames.DisplayNames(ctx, userIDs)
		if err != nil {
			h.Logger.WarnContext(ctx, "display_names_unavailable", slog.Any("error", err))
		}
		for _, m := range resp.Members {
			m.DisplayName = names[m.UserId]
		}
	}
	resp.NextPageToken = NextPageToken(len(resp.Members), limit, last)
	return connect.NewResponse(resp), nil
}

// loadMembershipForUpdate locks and returns the membership ID and role of
// userID.
func loadMembershipForUpdate(ctx context.Context, q db.Querier, orgID, userID string) (string, tidev1.Role, error) {
	var id string
	var role int16
	err := q.QueryRow(ctx, "SELECT membership_id::text, role FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE", orgID, userID).Scan(&id, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, ErrNotFound("member")
	}
	return id, tidev1.Role(role), err
}

func (h MemberHandler) UpdateMemberRole(ctx context.Context, req *connect.Request[tidev1.UpdateMemberRoleRequest]) (*connect.Response[tidev1.UpdateMemberRoleResponse], error) {
	target := req.Msg.GetUserId()
	role := req.Msg.GetRole()
	var resp *tidev1.UpdateMemberRoleResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageMembers)
		if err != nil {
			return err
		}
		if target == "" {
			return ErrInvalidf("invalid user_id: is required")
		}
		if role != tidev1.Role_ROLE_ADMIN && role != tidev1.Role_ROLE_MEMBER {
			return ErrInvalidf("invalid role: must be ROLE_ADMIN or ROLE_MEMBER; use TransferOwnership for the owner")
		}
		if err := LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		membershipID, current, err := loadMembershipForUpdate(ctx, tx, c.OrganizationID, target)
		if err != nil {
			return err
		}
		if current == tidev1.Role_ROLE_OWNER {
			return ErrPermission("the owner's role changes only with TransferOwnership")
		}
		// When the caller changes its own role, Authorize can have read a
		// role that a concurrent change replaced before the row lock above.
		// The locked role is the current one, so check the permission again
		// with it; otherwise this update could undo a demotion of the
		// caller. For another target the update is ordered before any later
		// change of the caller's role, so the first check is enough.
		if target == c.UserID && !authz.RoleAllowed(current, authz.ActionManageMembers) {
			return ErrPermission("the caller's role does not allow this action")
		}
		var joined time.Time
		if err := tx.QueryRow(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2 RETURNING join_time", c.OrganizationID, target, int16(role)).Scan(&joined); err != nil {
			return err
		}
		if current != role {
			if err := h.WriteAudit(ctx, tx, AuditEvent{
				OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_MEMBER_ROLE_CHANGED, Actor: c.Principal,
				TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID, SubjectUserID: target,
			}); err != nil {
				return err
			}
		}
		resp = &tidev1.UpdateMemberRoleResponse{Member: &tidev1.Member{OrganizationId: c.OrganizationID, UserId: target, Role: role, JoinTime: Timestamp(joined)}}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "update member role", err)
	}
	return connect.NewResponse(resp), nil
}

// RemoveMember ends a membership: removal by an owner or admin, or leaving.
// In one transaction it deletes the membership, revokes every personal API
// key of the user in the organization, runs the InTx membership end hooks
// and records the audit events; after the commit it runs the AfterCommit
// hooks (the dev session layer ends the user's dev sessions there).
func (h MemberHandler) RemoveMember(ctx context.Context, req *connect.Request[tidev1.RemoveMemberRequest]) (*connect.Response[tidev1.RemoveMemberResponse], error) {
	target := req.Msg.GetUserId()
	var end MembershipEnd
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, err := principal(ctx)
		if err != nil {
			return err
		}
		action := authz.ActionManageMembers
		reason := MembershipRemoved
		if target != "" && target == p.UserID {
			action, reason = authz.ActionLeave, MembershipLeft
		}
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), action)
		if err != nil {
			if action == authz.ActionLeave && c.Role == tidev1.Role_ROLE_OWNER {
				return ErrPrecondition("the owner must transfer ownership before leaving")
			}
			return err
		}
		if target == "" {
			return ErrInvalidf("invalid user_id: is required")
		}
		if err := LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		membershipID, role, err := loadMembershipForUpdate(ctx, tx, c.OrganizationID, target)
		if err != nil {
			return err
		}
		if role == tidev1.Role_ROLE_OWNER {
			return ErrPermission("the owner cannot be removed")
		}
		if _, err := tx.Exec(ctx, "DELETE FROM memberships WHERE membership_id = $1", membershipID); err != nil {
			return err
		}
		revoked, err := h.revokeAPIKeysTx(ctx, tx, c.OrganizationID, target, c.Principal)
		if err != nil {
			return err
		}
		end = MembershipEnd{
			OrganizationID: c.OrganizationID, UserID: target, MembershipID: membershipID, Reason: reason,
			Actor: c.Principal, RevokedAPIKeyIDs: revoked,
		}
		if err := h.membershipEndInTx(ctx, tx, end); err != nil {
			return err
		}
		return h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: tidev1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED, Actor: c.Principal,
			TargetKind: tidev1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID, SubjectUserID: target,
		})
	})
	if err != nil {
		return nil, h.Internal(ctx, "remove member", err)
	}
	h.membershipEndAfterCommit(ctx, end)
	return connect.NewResponse(&tidev1.RemoveMemberResponse{}), nil
}
