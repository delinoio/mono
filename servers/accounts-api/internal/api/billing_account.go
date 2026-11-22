// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/protos/delino/accounts/v1/accountsv1connect"
	"github.com/delinoio/mono/servers/accounts-api/internal/db"
)

// BillingAccountHandler implements BillingAccountService.
type BillingAccountHandler struct {
	*Service
}

var _ accountsv1connect.BillingAccountServiceHandler = (*BillingAccountHandler)(nil)

// billingAccount is a billing account row together with the caller's role.
type billingAccount struct {
	ID         string
	Name       string
	CreateTime time.Time
	Balance    int64
	CallerRole Role
}

func (a billingAccount) proto() *accountsv1.BillingAccount {
	return &accountsv1.BillingAccount{
		BillingAccountId: a.ID,
		Name:             a.Name,
		CallerRole:       a.CallerRole.Proto(),
		CreateTime:       timestamppb.New(a.CreateTime),
	}
}

// Lock modes of loadAccess.
type lockMode int

const (
	lockNone lockMode = iota
	// lockAccount locks the billing account row until the transaction ends.
	// Every change of members, invitations, the name or the deletion state
	// takes this lock first, so changes of one billing account serialize.
	lockAccount
)

const selectAccess = `SELECT a.billing_account_id::text, a.name, a.create_time, a.balance_usd_micros, m.role
	FROM billing_accounts a
	JOIN billing_account_members m ON m.billing_account_id = a.billing_account_id AND m.user_id = $2
	WHERE a.billing_account_id = $1 AND a.delete_time IS NULL`

// lockAccountRow locks the live billing account row until the transaction
// ends.
const lockAccountRow = `SELECT 1 FROM billing_accounts WHERE billing_account_id = $1 AND delete_time IS NULL FOR UPDATE`

// loadAccess returns the billing account and the caller's role. A missing or
// deleted billing account and a caller without a membership are all
// not_found, so callers cannot discover other billing accounts.
//
// With lockAccount the account row is locked in its own statement before the
// membership is read. A single statement that joins the membership and
// appends FOR UPDATE OF the account reads the unlocked membership row in the
// snapshot taken before the lock wait (READ COMMITTED does not re-read it),
// so a role changed by the transaction that held the lock would be missed.
// Every membership change takes the account lock first, so the second
// statement, which takes its snapshot after the lock is granted, sees the
// committed membership.
func loadAccess(ctx context.Context, q db.Querier, accountID, userID string, mode lockMode) (billingAccount, error) {
	if mode == lockAccount {
		var locked int
		err := q.QueryRow(ctx, lockAccountRow, accountID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return billingAccount{}, notFound("billing account not found")
		}
		if err != nil {
			return billingAccount{}, err
		}
	}
	var a billingAccount
	var role string
	err := q.QueryRow(ctx, selectAccess, accountID, userID).Scan(&a.ID, &a.Name, &a.CreateTime, &a.Balance, &role)
	if errors.Is(err, pgx.ErrNoRows) {
		return billingAccount{}, notFound("billing account not found")
	}
	if err != nil {
		return billingAccount{}, err
	}
	a.CallerRole = Role(role)
	return a, nil
}

// require returns permission_denied unless the role allows the action.
func require(a billingAccount, p Permission) error {
	if !a.CallerRole.Allows(p) {
		return permissionDenied("the caller's role in the billing account does not allow this action")
	}
	return nil
}

// CreateBillingAccount creates a billing account with the caller as owner.
func (h *BillingAccountHandler) CreateBillingAccount(ctx context.Context, req *connect.Request[accountsv1.CreateBillingAccountRequest]) (*connect.Response[accountsv1.CreateBillingAccountResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	name, err := billingAccountName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	id, err := newID()
	if err != nil {
		return nil, h.internal(ctx, "create_billing_account", err)
	}
	now := h.timestamp()
	audit := &auditLog{}
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO billing_accounts (billing_account_id, name, create_time, update_time)
			VALUES ($1, $2, $3, $3)`, id, name, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_account_members (billing_account_id, user_id, role, create_time)
			VALUES ($1, $2, $3, $4)`, id, userID, string(RoleOwner), now); err != nil {
			return err
		}
		return h.recordAudit(ctx, tx, audit, now, auditEvent{
			BillingAccountID: id, Kind: AuditBillingAccountCreated, ActorUserID: userID,
			Details: map[string]string{"name": name},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "create_billing_account", err)
	}
	h.flushAudit(ctx, audit)
	account := billingAccount{ID: id, Name: name, CreateTime: now, CallerRole: RoleOwner}
	return connect.NewResponse(&accountsv1.CreateBillingAccountResponse{BillingAccount: account.proto()}), nil
}

// GetBillingAccount reads one billing account. Every member may call it.
func (h *BillingAccountHandler) GetBillingAccount(ctx context.Context, req *connect.Request[accountsv1.GetBillingAccountRequest]) (*connect.Response[accountsv1.GetBillingAccountResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	account, err := loadAccess(ctx, h.db.Pool, accountID, userID, lockNone)
	if err != nil {
		return nil, h.internal(ctx, "get_billing_account", err)
	}
	if err := require(account, PermissionRead); err != nil {
		return nil, err
	}
	return connect.NewResponse(&accountsv1.GetBillingAccountResponse{BillingAccount: account.proto()}), nil
}

// ListBillingAccounts lists the caller's billing accounts by ID (creation
// order).
func (h *BillingAccountHandler) ListBillingAccounts(ctx context.Context, req *connect.Request[accountsv1.ListBillingAccountsRequest]) (*connect.Response[accountsv1.ListBillingAccountsResponse], error) {
	const rpc = "ListBillingAccounts"
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	size, err := pageSize(req.Msg.GetPageSize())
	if err != nil {
		return nil, err
	}
	tok, err := decodePageToken(req.Msg.GetPageToken(), rpc, userID)
	if err != nil {
		return nil, err
	}
	after := "00000000-0000-0000-0000-000000000000"
	if tok.After != "" {
		if after, err = normalizeID("page_token", tok.After); err != nil {
			return nil, invalidArgument("page_token is not valid for this request")
		}
	}
	rows, err := h.db.Pool.Query(ctx, `SELECT a.billing_account_id::text, a.name, a.create_time, a.balance_usd_micros, m.role
		FROM billing_account_members m
		JOIN billing_accounts a ON a.billing_account_id = m.billing_account_id
		WHERE m.user_id = $1 AND a.delete_time IS NULL AND a.billing_account_id > $2
		ORDER BY a.billing_account_id
		LIMIT $3`, userID, after, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_billing_accounts", err)
	}
	accounts, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (billingAccount, error) {
		var a billingAccount
		var role string
		err := row.Scan(&a.ID, &a.Name, &a.CreateTime, &a.Balance, &role)
		a.CallerRole = Role(role)
		return a, err
	})
	if err != nil {
		return nil, h.internal(ctx, "list_billing_accounts", err)
	}
	resp := &accountsv1.ListBillingAccountsResponse{}
	if len(accounts) > size {
		accounts = accounts[:size]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: userID, After: accounts[size-1].ID})
	}
	for _, a := range accounts {
		resp.BillingAccounts = append(resp.BillingAccounts, a.proto())
	}
	return connect.NewResponse(resp), nil
}

// UpdateBillingAccount renames a billing account. Owner and admin only.
func (h *BillingAccountHandler) UpdateBillingAccount(ctx context.Context, req *connect.Request[accountsv1.UpdateBillingAccountRequest]) (*connect.Response[accountsv1.UpdateBillingAccountResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	name, err := billingAccountName(req.Msg.GetName())
	if err != nil {
		return nil, err
	}
	now := h.timestamp()
	var account billingAccount
	audit := &auditLog{}
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err = loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionManage); err != nil {
			return err
		}
		if account.Name == name {
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_accounts SET name = $2, update_time = $3 WHERE billing_account_id = $1`,
			accountID, name, now); err != nil {
			return err
		}
		oldName := account.Name
		account.Name = name
		return h.recordAudit(ctx, tx, audit, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditBillingAccountRenamed, ActorUserID: userID,
			Details: map[string]string{"old_name": oldName, "new_name": name},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "update_billing_account", err)
	}
	h.flushAudit(ctx, audit)
	return connect.NewResponse(&accountsv1.UpdateBillingAccountResponse{BillingAccount: account.proto()}), nil
}

// DeleteBillingAccount soft-deletes a billing account. Owner only.
func (h *BillingAccountHandler) DeleteBillingAccount(ctx context.Context, req *connect.Request[accountsv1.DeleteBillingAccountRequest]) (*connect.Response[accountsv1.DeleteBillingAccountResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	now := h.timestamp()
	audit := &auditLog{}
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionOwn); err != nil {
			return err
		}
		if account.Balance < 0 {
			return failedPrecondition("a billing account with a negative balance cannot be deleted")
		}
		var linked bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organization_links
			WHERE billing_account_id = $1 AND unlink_time IS NULL)`, accountID).Scan(&linked); err != nil {
			return err
		}
		if linked {
			return failedPrecondition("a billing account with linked organizations cannot be deleted")
		}
		// Cancel the Paddle subscription at once and forfeit the remaining
		// credit with EXPIRY entries. A Paddle failure fails the deletion,
		// so nothing changes. The deletion is a soft delete: the row, the
		// ledger, the grants, the organization link history and the audit
		// events stay for the retention period.
		if err := h.billing.CloseAccount(ctx, tx, accountID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_accounts SET delete_time = $2, update_time = $2 WHERE billing_account_id = $1`,
			accountID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_account_invitations SET revoke_time = $2
			WHERE billing_account_id = $1 AND accept_time IS NULL AND revoke_time IS NULL`, accountID, now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM billing_account_members WHERE billing_account_id = $1`, accountID); err != nil {
			return err
		}
		return h.recordAudit(ctx, tx, audit, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditBillingAccountDeleted, ActorUserID: userID,
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "delete_billing_account", err)
	}
	h.flushAudit(ctx, audit)
	return connect.NewResponse(&accountsv1.DeleteBillingAccountResponse{}), nil
}
