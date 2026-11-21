// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
)

// displayNameConcurrency bounds the parallel Logto profile reads of one
// ListMembers call, and displayNameTimeout the time that they may take
// together. A name that is not read in time stays empty.
const (
	displayNameConcurrency = 8
	displayNameTimeout     = 5 * time.Second
)

// member is a membership row.
type member struct {
	UserID     string
	Role       Role
	CreateTime time.Time
}

func (m member) proto(accountID, displayName string) *accountsv1.BillingAccountMember {
	return &accountsv1.BillingAccountMember{
		BillingAccountId: accountID,
		UserId:           m.UserID,
		Role:             m.Role.Proto(),
		DisplayName:      displayName,
		CreateTime:       timestamppb.New(m.CreateTime),
	}
}

// loadMember returns the membership of userID, locked for update, or
// not_found.
func loadMember(ctx context.Context, tx pgx.Tx, accountID, userID string) (member, error) {
	var m member
	var role string
	err := tx.QueryRow(ctx, `SELECT user_id, role, create_time FROM billing_account_members
		WHERE billing_account_id = $1 AND user_id = $2 FOR UPDATE`, accountID, userID).Scan(&m.UserID, &role, &m.CreateTime)
	if errors.Is(err, pgx.ErrNoRows) {
		return member{}, notFound("member not found")
	}
	m.Role = Role(role)
	return m, err
}

// displayNames reads the Logto display names of the users. A name that
// cannot be read is empty, as the contract allows.
func (h *BillingAccountHandler) displayNames(ctx context.Context, userIDs []string) []string {
	names := make([]string, len(userIDs))
	ctx, cancel := context.WithTimeout(ctx, displayNameTimeout)
	defer cancel()
	sem := make(chan struct{}, displayNameConcurrency)
	var wg sync.WaitGroup
	var failures int
	var mu sync.Mutex
	for i, id := range userIDs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			user, err := h.directory.User(ctx, id)
			if err != nil {
				if !errors.Is(err, directory.ErrUserNotFound) {
					mu.Lock()
					failures++
					mu.Unlock()
				}
				return
			}
			names[i] = user.Name
		}()
	}
	wg.Wait()
	if failures > 0 {
		h.logger.WarnContext(ctx, "logto_display_names_unavailable", slog.Int("failures", failures))
	}
	return names
}

// ListMembers lists the members in join order. Every member may call it.
func (h *BillingAccountHandler) ListMembers(ctx context.Context, req *connect.Request[accountsv1.ListMembersRequest]) (*connect.Response[accountsv1.ListMembersResponse], error) {
	const rpc = "ListMembers"
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, accountID)
	if err != nil {
		return nil, err
	}
	account, err := loadAccess(ctx, h.db.Pool, accountID, userID, lockNone)
	if err != nil {
		return nil, h.internal(ctx, "list_members", err)
	}
	if err := require(account, PermissionRead); err != nil {
		return nil, err
	}
	// The sort key is (create_time, user_id). The first page starts before
	// every key: no join time is earlier than the Unix epoch.
	afterTime := time.Unix(0, 0).UTC()
	afterUser := ""
	if tok.After != "" {
		afterTime = time.UnixMicro(tok.AfterTime).UTC()
		afterUser = tok.After
	}
	rows, err := h.db.Pool.Query(ctx, `SELECT user_id, role, create_time FROM billing_account_members
		WHERE billing_account_id = $1 AND (create_time, user_id) > ($2, $3)
		ORDER BY create_time, user_id
		LIMIT $4`, accountID, afterTime, afterUser, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_members", err)
	}
	members, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (member, error) {
		var m member
		var role string
		err := row.Scan(&m.UserID, &role, &m.CreateTime)
		m.Role = Role(role)
		return m, err
	})
	if err != nil {
		return nil, h.internal(ctx, "list_members", err)
	}
	resp := &accountsv1.ListMembersResponse{}
	if len(members) > size {
		members = members[:size]
		last := members[size-1]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: accountID, After: last.UserID, AfterTime: last.CreateTime.UnixMicro()})
	}
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.UserID
	}
	names := h.displayNames(ctx, ids)
	for i, m := range members {
		resp.Members = append(resp.Members, m.proto(accountID, names[i]))
	}
	return connect.NewResponse(resp), nil
}

// UpdateMemberRole changes a member between ADMIN and VIEWER. Owner and admin
// only; the owner's role cannot change.
func (h *BillingAccountHandler) UpdateMemberRole(ctx context.Context, req *connect.Request[accountsv1.UpdateMemberRoleRequest]) (*connect.Response[accountsv1.UpdateMemberRoleResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	targetID, err := userIDField(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	role, ok := grantableRole(req.Msg.GetRole())
	if !ok {
		return nil, invalidArgument("role must be ADMIN or VIEWER")
	}
	now := h.timestamp()
	var target member
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionManage); err != nil {
			return err
		}
		target, err = loadMember(ctx, tx, accountID, targetID)
		if err != nil {
			return err
		}
		if target.Role == RoleOwner {
			return failedPrecondition("the owner's role cannot be changed; transfer the ownership instead")
		}
		if target.Role == role {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_account_members SET role = $3 WHERE billing_account_id = $1 AND user_id = $2`,
			accountID, targetID, string(role)); err != nil {
			return err
		}
		oldRole := target.Role
		target.Role = role
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditMemberRoleChanged, ActorUserID: userID, TargetUserID: targetID,
			Details: map[string]string{"old_role": string(oldRole), "new_role": string(role)},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "update_member_role", err)
	}
	names := h.displayNames(ctx, []string{targetID})
	return connect.NewResponse(&accountsv1.UpdateMemberRoleResponse{Member: target.proto(accountID, names[0])}), nil
}

// RemoveMember removes an admin or a viewer. Owners and admins remove any
// admin or viewer; every admin and viewer may remove itself.
func (h *BillingAccountHandler) RemoveMember(ctx context.Context, req *connect.Request[accountsv1.RemoveMemberRequest]) (*connect.Response[accountsv1.RemoveMemberResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	targetID, err := userIDField(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	now := h.timestamp()
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if targetID != userID {
			if err := require(account, PermissionManage); err != nil {
				return err
			}
		}
		target, err := loadMember(ctx, tx, accountID, targetID)
		if err != nil {
			return err
		}
		if target.Role == RoleOwner {
			return failedPrecondition("the owner cannot be removed; transfer the ownership first")
		}
		// A change of members does not change the organization links of the
		// billing account.
		if _, err := tx.Exec(ctx, `DELETE FROM billing_account_members WHERE billing_account_id = $1 AND user_id = $2`,
			accountID, targetID); err != nil {
			return err
		}
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditMemberRemoved, ActorUserID: userID, TargetUserID: targetID,
			Details: map[string]string{"role": string(target.Role)},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "remove_member", err)
	}
	return connect.NewResponse(&accountsv1.RemoveMemberResponse{}), nil
}

// TransferOwnership makes another member the owner; the previous owner
// becomes an admin. Owner only.
func (h *BillingAccountHandler) TransferOwnership(ctx context.Context, req *connect.Request[accountsv1.TransferOwnershipRequest]) (*connect.Response[accountsv1.TransferOwnershipResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	targetID, err := userIDField(req.Msg.GetUserId())
	if err != nil {
		return nil, err
	}
	now := h.timestamp()
	var account billingAccount
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err = loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionOwn); err != nil {
			return err
		}
		if targetID == userID {
			return invalidArgument("user_id must name another member")
		}
		target, err := loadMember(ctx, tx, accountID, targetID)
		if err != nil {
			return err
		}
		// Demote first: the schema allows one owner per billing account at
		// any time.
		if _, err := tx.Exec(ctx, `UPDATE billing_account_members SET role = $3 WHERE billing_account_id = $1 AND user_id = $2`,
			accountID, userID, string(RoleAdmin)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_account_members SET role = $3 WHERE billing_account_id = $1 AND user_id = $2`,
			accountID, targetID, string(RoleOwner)); err != nil {
			return err
		}
		account.CallerRole = RoleAdmin
		return h.recordAudit(ctx, tx, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditOwnershipTransferred, ActorUserID: userID, TargetUserID: targetID,
			Details: map[string]string{"new_owner_previous_role": string(target.Role)},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "transfer_ownership", err)
	}
	return connect.NewResponse(&accountsv1.TransferOwnershipResponse{BillingAccount: account.proto()}), nil
}
