// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// MemberHandler serves MemberService.
type MemberHandler struct{ *Service }

var _ ownsandv1connect.MemberServiceHandler = MemberHandler{}

const invitationColumns = `invitation_id::text, organization_id::text, role, secret_prefix, created_by_kind, created_by_id,
	create_time, expire_time, accept_time, revoke_time`

func (s *Service) scanInvitation(row pgx.Row) (*ownsandv1.Invitation, error) {
	var inv ownsandv1.Invitation
	var role, kind int16
	var createdBy string
	var created, expires time.Time
	var accepted, revoked *time.Time
	if err := row.Scan(&inv.InvitationId, &inv.OrganizationId, &role, &inv.SecretPrefix, &kind, &createdBy, &created, &expires, &accepted, &revoked); err != nil {
		return nil, err
	}
	inv.Role = ownsandv1.Role(role)
	inv.CreatedBy = principalProto(kind, createdBy)
	inv.CreateTime = ts(created)
	inv.ExpireTime = ts(expires)
	switch {
	case accepted != nil:
		inv.State = ownsandv1.InvitationState_INVITATION_STATE_ACCEPTED
	case revoked != nil:
		inv.State = ownsandv1.InvitationState_INVITATION_STATE_REVOKED
	case !s.now().Before(expires):
		inv.State = ownsandv1.InvitationState_INVITATION_STATE_EXPIRED
	default:
		inv.State = ownsandv1.InvitationState_INVITATION_STATE_PENDING
	}
	return &inv, nil
}

func (h MemberHandler) CreateInvitation(ctx context.Context, req *connect.Request[ownsandv1.CreateInvitationRequest]) (*connect.Response[ownsandv1.CreateInvitationResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	role := req.Msg.GetRole()
	var resp *ownsandv1.CreateInvitationResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageMembers)
		if err != nil {
			return err
		}
		if role != ownsandv1.Role_ROLE_ADMIN && role != ownsandv1.Role_ROLE_MEMBER {
			return errInvalidf("invalid role: must be ROLE_ADMIN or ROLE_MEMBER")
		}
		secret, err := auth.NewSecret(auth.SecretInvitation)
		if err != nil {
			return err
		}
		id := newID()
		now := h.now()
		row := tx.QueryRow(ctx, `INSERT INTO invitations (invitation_id, organization_id, role, secret_sha256, secret_prefix, created_by_kind, created_by_id, create_time, expire_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING `+invitationColumns,
			id, orgID, int16(role), secret.Hash, secret.DisplayPrefix, int16(p.Kind), p.ID, now, now.Add(invitationLifetime))
		inv, err := h.scanInvitation(row)
		if err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_CREATED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION, TargetID: id,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.CreateInvitationResponse{Invitation: inv, InvitationSecret: secret.Value}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "create invitation", err)
	}
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) ListInvitations(ctx context.Context, req *connect.Request[ownsandv1.ListInvitationsRequest]) (*connect.Response[ownsandv1.ListInvitationsResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionManageMembers); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT `+invitationColumns+` FROM invitations
		WHERE organization_id = $1 AND invitation_id > $2 AND accept_time IS NULL AND revoke_time IS NULL AND expire_time > $3
		ORDER BY invitation_id LIMIT $4`, orgID, afterKey(after), h.now(), limit)
	if err != nil {
		return nil, h.internal(ctx, "list invitations", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListInvitationsResponse{}
	for rows.Next() {
		inv, err := h.scanInvitation(rows)
		if err != nil {
			return nil, h.internal(ctx, "list invitations", err)
		}
		resp.Invitations = append(resp.Invitations, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list invitations", err)
	}
	last := ""
	if n := len(resp.Invitations); n > 0 {
		last = resp.Invitations[n-1].GetInvitationId()
	}
	resp.NextPageToken = nextPageToken(len(resp.Invitations), limit, last)
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) RevokeInvitation(ctx context.Context, req *connect.Request[ownsandv1.RevokeInvitationRequest]) (*connect.Response[ownsandv1.RevokeInvitationResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	invID := req.Msg.GetInvitationId()
	var resp *ownsandv1.RevokeInvitationResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageMembers)
		if err != nil {
			return err
		}
		if err := validateID("invitation_id", invID); err != nil {
			return err
		}
		inv, err := h.scanInvitation(tx.QueryRow(ctx, "SELECT "+invitationColumns+" FROM invitations WHERE organization_id = $1 AND invitation_id = $2 FOR UPDATE", orgID, invID))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("invitation")
		}
		if err != nil {
			return err
		}
		if inv.GetState() != ownsandv1.InvitationState_INVITATION_STATE_PENDING {
			return errPrecondition("only a pending invitation can be revoked")
		}
		inv, err = h.scanInvitation(tx.QueryRow(ctx, "UPDATE invitations SET revoke_time = $3 WHERE organization_id = $1 AND invitation_id = $2 RETURNING "+invitationColumns, orgID, invID, h.now()))
		if err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_INVITATION_REVOKED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_INVITATION, TargetID: invID,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.RevokeInvitationResponse{Invitation: inv}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "revoke invitation", err)
	}
	return connect.NewResponse(resp), nil
}

// errInvitationInvalid is the one answer for an unknown, expired, revoked
// or used invitation, so the response does not show which condition failed.
func errInvitationInvalid() error { return errNotFound("invitation") }

func (h MemberHandler) AcceptInvitation(ctx context.Context, req *connect.Request[ownsandv1.AcceptInvitationRequest]) (*connect.Response[ownsandv1.AcceptInvitationResponse], error) {
	p, err := requireUser(ctx)
	if err != nil {
		return nil, err
	}
	secret := req.Msg.GetInvitationSecret()
	prefix := auth.DisplayPrefix(secret)
	if ok, retry := h.Limits.PerPrefix.Allow("invitation:"+prefix, 1); !ok {
		return nil, errRateLimited(retry)
	}
	if ok, retry := h.Limits.PerDevice.Allow("invitation-user:"+p.ID, 1); !ok {
		return nil, errRateLimited(retry)
	}
	if addr := clientAddr(ctx); addr != "" {
		if ok, retry := h.Limits.PerClient.Allow("invitation:"+addr, 1); !ok {
			return nil, errRateLimited(retry)
		}
	}
	if !auth.WellFormed(auth.SecretInvitation, secret) {
		return nil, errInvitationInvalid()
	}
	hash := auth.HashSecret(secret)
	var resp *ownsandv1.AcceptInvitationResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		var invID, orgID, orgName string
		var role int16
		var stored []byte
		var orgCreated time.Time
		err := tx.QueryRow(ctx, `SELECT i.invitation_id::text, i.organization_id::text, i.role, i.secret_sha256, o.name, o.create_time
			FROM invitations i JOIN organizations o ON o.organization_id = i.organization_id
			WHERE i.secret_sha256 = $1 AND i.accept_time IS NULL AND i.revoke_time IS NULL AND i.expire_time > $2
			FOR UPDATE OF i`, hash, h.now()).Scan(&invID, &orgID, &role, &stored, &orgName, &orgCreated)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && !auth.HashEqual(stored, hash)) {
			return errInvitationInvalid()
		}
		if err != nil {
			return err
		}
		setRPCOrganization(ctx, orgID)
		var exists bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM memberships WHERE organization_id = $1 AND user_id = $2)", orgID, p.ID).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return errAlreadyExists("the caller is already a member of the organization")
		}
		now := h.now()
		membershipID := newID()
		// Two concurrent acceptances by the same user can both pass the
		// existence check above. ON CONFLICT turns the loser's unique
		// violation into zero affected rows, so it gets already_exists
		// instead of an internal error. Other SQL errors stay internal.
		tag, err := tx.Exec(ctx, "INSERT INTO memberships (membership_id, organization_id, user_id, role, join_time) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (organization_id, user_id) DO NOTHING",
			membershipID, orgID, p.ID, role, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errAlreadyExists("the caller is already a member of the organization")
		}
		if _, err := tx.Exec(ctx, "UPDATE invitations SET accept_time = $2, accepted_by = $3 WHERE invitation_id = $1", invID, now, p.ID); err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_JOINED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.AcceptInvitationResponse{
			Organization: organizationProto(orgID, orgName, orgCreated, ownsandv1.Role(role)),
			Member:       &ownsandv1.Member{OrganizationId: orgID, UserId: p.ID, Role: ownsandv1.Role(role), JoinTime: ts(now)},
		}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "accept invitation", err)
	}
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) ListMembers(ctx context.Context, req *connect.Request[ownsandv1.ListMembersRequest]) (*connect.Response[ownsandv1.ListMembersResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadOrganization); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT membership_id::text, user_id, role, join_time FROM memberships
		WHERE organization_id = $1 AND membership_id > $2 ORDER BY membership_id LIMIT $3`, orgID, afterKey(after), limit)
	if err != nil {
		return nil, h.internal(ctx, "list members", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListMembersResponse{}
	last := ""
	var userIDs []string
	for rows.Next() {
		var m ownsandv1.Member
		var role int16
		var joined time.Time
		if err := rows.Scan(&last, &m.UserId, &role, &joined); err != nil {
			return nil, h.internal(ctx, "list members", err)
		}
		m.OrganizationId = orgID
		m.Role = ownsandv1.Role(role)
		m.JoinTime = ts(joined)
		resp.Members = append(resp.Members, &m)
		userIDs = append(userIDs, m.UserId)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list members", err)
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
	resp.NextPageToken = nextPageToken(len(resp.Members), limit, last)
	return connect.NewResponse(resp), nil
}

// loadMembership returns the membership ID and role of userID.
func loadMembership(ctx context.Context, q db.Querier, orgID, userID string) (string, ownsandv1.Role, error) {
	var id string
	var role int16
	err := q.QueryRow(ctx, "SELECT membership_id::text, role FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR UPDATE", orgID, userID).Scan(&id, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, errNotFound("member")
	}
	return id, ownsandv1.Role(role), err
}

func (h MemberHandler) UpdateMemberRole(ctx context.Context, req *connect.Request[ownsandv1.UpdateMemberRoleRequest]) (*connect.Response[ownsandv1.UpdateMemberRoleResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	target := req.Msg.GetUserId()
	role := req.Msg.GetRole()
	var resp *ownsandv1.UpdateMemberRoleResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageMembers)
		if err != nil {
			return err
		}
		if target == "" {
			return errInvalidf("invalid user_id: is required")
		}
		if role != ownsandv1.Role_ROLE_ADMIN && role != ownsandv1.Role_ROLE_MEMBER {
			return errInvalidf("invalid role: must be ROLE_ADMIN or ROLE_MEMBER; use TransferOwnership for the owner")
		}
		membershipID, current, err := loadMembership(ctx, tx, orgID, target)
		if err != nil {
			return err
		}
		if current == ownsandv1.Role_ROLE_OWNER {
			return errPermission("the owner's role changes only with TransferOwnership")
		}
		var joined time.Time
		if err := tx.QueryRow(ctx, "UPDATE memberships SET role = $3 WHERE organization_id = $1 AND user_id = $2 RETURNING join_time", orgID, target, int16(role)).Scan(&joined); err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_ROLE_CHANGED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.UpdateMemberRoleResponse{Member: &ownsandv1.Member{OrganizationId: orgID, UserId: target, Role: role, JoinTime: ts(joined)}}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "update member role", err)
	}
	return connect.NewResponse(resp), nil
}

func (h MemberHandler) RemoveMember(ctx context.Context, req *connect.Request[ownsandv1.RemoveMemberRequest]) (*connect.Response[ownsandv1.RemoveMemberResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	target := req.Msg.GetUserId()
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, err := principal(ctx)
		if err != nil {
			return err
		}
		action := authz.ActionManageMembers
		if p.IsUser() && target == p.ID {
			action = authz.ActionLeave
		}
		p, callerRole, err := h.authorize(ctx, tx, orgID, action)
		if err != nil {
			if action == authz.ActionLeave && callerRole == ownsandv1.Role_ROLE_OWNER {
				return errPrecondition("the owner must transfer ownership before leaving")
			}
			return err
		}
		if target == "" {
			return errInvalidf("invalid user_id: is required")
		}
		membershipID, role, err := loadMembership(ctx, tx, orgID, target)
		if err != nil {
			return err
		}
		if role == ownsandv1.Role_ROLE_OWNER {
			return errPermission("the owner cannot be removed")
		}
		if _, err := tx.Exec(ctx, "DELETE FROM memberships WHERE organization_id = $1 AND user_id = $2", orgID, target); err != nil {
			return err
		}
		if err := events.Write(ctx, tx, events.Event{Kind: events.KindMembershipEnded, OrganizationID: orgID, UserID: target}); err != nil {
			return err
		}
		return h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_MEMBER_REMOVED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_MEMBERSHIP, TargetID: membershipID,
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "remove member", err)
	}
	h.notify()
	return connect.NewResponse(&ownsandv1.RemoveMemberResponse{}), nil
}
