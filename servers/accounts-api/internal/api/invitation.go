// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/timestamppb"

	accountsv1 "github.com/delinoio/mono/protos/delino/accounts/v1"
	"github.com/delinoio/mono/servers/accounts-api/internal/directory"
	"github.com/delinoio/mono/servers/accounts-api/internal/email"
)

// Invitation token format: the prefix followed by 32 random bytes in unpadded
// base64url. The server stores only the SHA-256 hash of the whole token.
const (
	invitationTokenPrefix = "bai_"
	invitationTokenBytes  = 32
	// InvitationLifetime is the time from creation to expiry.
	InvitationLifetime = 7 * 24 * time.Hour
)

// newInvitationToken returns a new token and its hash.
func newInvitationToken() (string, []byte, error) {
	var raw [invitationTokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", nil, err
	}
	token := invitationTokenPrefix + base64.RawURLEncoding.EncodeToString(raw[:])
	return token, hashInvitationToken(token), nil
}

func hashInvitationToken(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// wellFormedInvitationToken reports whether token has the shape of an
// invitation token. Other values cannot match a stored hash.
func wellFormedInvitationToken(token string) bool {
	body, ok := strings.CutPrefix(token, invitationTokenPrefix)
	if !ok {
		return false
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	return err == nil && len(raw) == invitationTokenBytes
}

// invitation is an invitation row.
type invitation struct {
	ID               string
	BillingAccountID string
	Email            string
	Role             Role
	InviterUserID    string
	CreateTime       time.Time
	ExpireTime       time.Time
	AcceptTime       *time.Time
	RevokeTime       *time.Time
}

// state returns the invitation state at now.
func (i invitation) state(now time.Time) accountsv1.InvitationState {
	switch {
	case i.AcceptTime != nil:
		return accountsv1.InvitationState_INVITATION_STATE_ACCEPTED
	case i.RevokeTime != nil:
		return accountsv1.InvitationState_INVITATION_STATE_REVOKED
	case !now.Before(i.ExpireTime):
		return accountsv1.InvitationState_INVITATION_STATE_EXPIRED
	default:
		return accountsv1.InvitationState_INVITATION_STATE_PENDING
	}
}

func (i invitation) proto(now time.Time) *accountsv1.BillingAccountInvitation {
	return &accountsv1.BillingAccountInvitation{
		InvitationId:     i.ID,
		BillingAccountId: i.BillingAccountID,
		Email:            i.Email,
		Role:             i.Role.Proto(),
		State:            i.state(now),
		InviterUserId:    i.InviterUserID,
		CreateTime:       timestamppb.New(i.CreateTime),
		ExpireTime:       timestamppb.New(i.ExpireTime),
	}
}

const invitationColumns = `invitation_id::text, billing_account_id::text, email, role, inviter_user_id,
	create_time, expire_time, accept_time, revoke_time`

func scanInvitation(row pgx.Row) (invitation, error) {
	var i invitation
	var role string
	err := row.Scan(&i.ID, &i.BillingAccountID, &i.Email, &role, &i.InviterUserID,
		&i.CreateTime, &i.ExpireTime, &i.AcceptTime, &i.RevokeTime)
	i.Role = Role(role)
	return i, err
}

// Why an undelivered invitation is revoked. The value is stored in the
// details of the invitation_revoked audit event.
const (
	revokeReasonDeliveryFailed = "delivery_failed"
	revokeReasonAuditFailed    = "audit_failed"
)

// invitationFollowUpTimeout bounds the transactions that run after the email
// was sent or failed. They run detached from the request context, so a client
// that disconnects during the send does not leave an undelivered invitation
// pending.
const invitationFollowUpTimeout = 10 * time.Second

// CreateInvitation invites an email address and sends the invitation email.
// Owner and admin only.
//
// The email is sent only after the transaction that inserts the invitation has
// committed, so a rolled-back transaction never sends an email for an
// invitation that does not exist. A second transaction then either records the
// invitation_created audit event (the email was accepted) or revokes the
// invitation under the account lock (the send failed) and the RPC fails with
// unavailable. A failed send therefore leaves a revoked invitation, which no
// longer blocks a new invitation for the address, and no invitation_created
// audit event. The Resend idempotency key `invitation/<invitation_id>` keeps a
// retried send of the same invitation to one email. A crash between the commit
// and the send leaves a pending invitation without an email; an owner or admin
// revokes it and invites again.
func (h *BillingAccountHandler) CreateInvitation(ctx context.Context, req *connect.Request[accountsv1.CreateInvitationRequest]) (*connect.Response[accountsv1.CreateInvitationResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	addr, err := invitationEmail(req.Msg.GetEmail())
	if err != nil {
		return nil, err
	}
	role, ok := grantableRole(req.Msg.GetRole())
	if !ok {
		return nil, invalidArgument("role must be ADMIN or VIEWER")
	}
	id, err := newID()
	if err != nil {
		return nil, h.internal(ctx, "create_invitation", err)
	}
	token, hash, err := newInvitationToken()
	if err != nil {
		return nil, h.internal(ctx, "create_invitation", err)
	}
	now := h.timestamp()
	inv := invitation{
		ID: id, BillingAccountID: accountID, Email: addr, Role: role, InviterUserID: userID,
		CreateTime: now, ExpireTime: now.Add(InvitationLifetime),
	}
	var msg email.Message
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionManage); err != nil {
			return err
		}
		var pending bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_account_invitations
			WHERE billing_account_id = $1 AND email_key = $2 AND accept_time IS NULL AND revoke_time IS NULL AND expire_time > $3)`,
			accountID, emailKey(addr), now).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return alreadyExists("a pending invitation for this email address exists")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_account_invitations
			(invitation_id, billing_account_id, token_sha256, email, email_key, role, inviter_user_id, create_time, expire_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
			id, accountID, hash, addr, emailKey(addr), string(role), userID, inv.CreateTime, inv.ExpireTime); err != nil {
			return err
		}
		msg, err = email.InvitationMessage(email.Invitation{
			InvitationID:       id,
			BillingAccountID:   accountID,
			BillingAccountName: account.Name,
			Role:               string(role),
			To:                 addr,
			Link:               email.InvitationLink(h.webOrigin, token),
			ExpireTime:         inv.ExpireTime,
		})
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "create_invitation", err)
	}

	// The invitation is committed. Send the email outside any transaction.
	if err := h.email.Send(ctx, msg); err != nil {
		h.logger.WarnContext(ctx, "invitation_email_failed",
			slog.String("billing_account_id", accountID), slog.String("invitation_id", id))
		h.revokeUndelivered(ctx, inv, userID, revokeReasonDeliveryFailed)
		return nil, unavailable("the invitation email could not be sent; try again later")
	}

	// The email was accepted: record the creation. If that fails, the
	// invitation is revoked so that every pending invitation has its
	// invitation_created audit event.
	followUp, cancel := invitationFollowUpContext(ctx)
	defer cancel()
	audit := &auditLog{}
	if err := h.db.InTx(followUp, func(tx pgx.Tx) error {
		return h.recordAudit(followUp, tx, audit, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditInvitationCreated, ActorUserID: userID, InvitationID: id,
			Details: map[string]string{"role": string(role)},
		})
	}); err != nil {
		h.revokeUndelivered(ctx, inv, userID, revokeReasonAuditFailed)
		return nil, h.internal(ctx, "create_invitation", err)
	}
	h.flushAudit(ctx, audit)
	return connect.NewResponse(&accountsv1.CreateInvitationResponse{Invitation: inv.proto(now)}), nil
}

// invitationFollowUpContext returns a bounded context that keeps the values of
// ctx but ignores its cancellation.
func invitationFollowUpContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), invitationFollowUpTimeout)
}

// revokeUndelivered revokes the invitation that CreateInvitation committed but
// could not deliver, under the billing account lock, and records the
// invitation_revoked audit event with the reason. Nothing happens when the
// invitation is no longer pending, for example because an owner revoked it or
// the billing account was deleted meanwhile. A failure is logged and not
// returned: the invitation then stays pending until an owner or admin revokes
// it.
func (h *BillingAccountHandler) revokeUndelivered(ctx context.Context, inv invitation, actorUserID, reason string) {
	followUp, cancel := invitationFollowUpContext(ctx)
	defer cancel()
	now := h.timestamp()
	audit := &auditLog{}
	err := h.db.InTx(followUp, func(tx pgx.Tx) error {
		var locked bool
		err := tx.QueryRow(followUp, `SELECT true FROM billing_accounts
			WHERE billing_account_id = $1 AND delete_time IS NULL FOR UPDATE`, inv.BillingAccountID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		tag, err := tx.Exec(followUp, `UPDATE billing_account_invitations SET revoke_time = $2
			WHERE invitation_id = $1 AND accept_time IS NULL AND revoke_time IS NULL`, inv.ID, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return nil
		}
		return h.recordAudit(followUp, tx, audit, now, auditEvent{
			BillingAccountID: inv.BillingAccountID, Kind: AuditInvitationRevoked, ActorUserID: actorUserID,
			InvitationID: inv.ID, Details: map[string]string{"reason": reason},
		})
	})
	if err != nil {
		h.logger.ErrorContext(ctx, "invitation_revoke_failed",
			slog.String("billing_account_id", inv.BillingAccountID), slog.String("invitation_id", inv.ID),
			slog.String("reason", reason), slog.Any("error", err))
		return
	}
	h.flushAudit(ctx, audit)
}

// ListInvitations lists the pending invitations, newest first. Owner and
// admin only.
func (h *BillingAccountHandler) ListInvitations(ctx context.Context, req *connect.Request[accountsv1.ListInvitationsRequest]) (*connect.Response[accountsv1.ListInvitationsResponse], error) {
	const rpc = "ListInvitations"
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
	before := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if tok.After != "" {
		if before, err = normalizeID("page_token", tok.After); err != nil {
			return nil, invalidArgument("page_token is not valid for this request")
		}
	}
	account, err := loadAccess(ctx, h.db.Pool, accountID, userID, lockNone)
	if err != nil {
		return nil, h.internal(ctx, "list_invitations", err)
	}
	if err := require(account, PermissionManage); err != nil {
		return nil, err
	}
	now := h.timestamp()
	rows, err := h.db.Pool.Query(ctx, `SELECT `+invitationColumns+` FROM billing_account_invitations
		WHERE billing_account_id = $1 AND accept_time IS NULL AND revoke_time IS NULL AND expire_time > $2
			AND invitation_id < $3
		ORDER BY invitation_id DESC
		LIMIT $4`, accountID, now, before, size+1)
	if err != nil {
		return nil, h.internal(ctx, "list_invitations", err)
	}
	invitations, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (invitation, error) { return scanInvitation(row) })
	if err != nil {
		return nil, h.internal(ctx, "list_invitations", err)
	}
	resp := &accountsv1.ListInvitationsResponse{}
	if len(invitations) > size {
		invitations = invitations[:size]
		resp.NextPageToken = encodePageToken(pageToken{RPC: rpc, Scope: accountID, After: invitations[size-1].ID})
	}
	for _, i := range invitations {
		resp.Invitations = append(resp.Invitations, i.proto(now))
	}
	return connect.NewResponse(resp), nil
}

// RevokeInvitation revokes a pending invitation. Owner and admin only.
func (h *BillingAccountHandler) RevokeInvitation(ctx context.Context, req *connect.Request[accountsv1.RevokeInvitationRequest]) (*connect.Response[accountsv1.RevokeInvitationResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	accountID, err := normalizeID("billing_account_id", req.Msg.GetBillingAccountId())
	if err != nil {
		return nil, err
	}
	invitationID, err := normalizeID("invitation_id", req.Msg.GetInvitationId())
	if err != nil {
		return nil, err
	}
	now := h.timestamp()
	var inv invitation
	audit := &auditLog{}
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		account, err := loadAccess(ctx, tx, accountID, userID, lockAccount)
		if err != nil {
			return err
		}
		if err := require(account, PermissionManage); err != nil {
			return err
		}
		inv, err = scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+` FROM billing_account_invitations
			WHERE invitation_id = $1 AND billing_account_id = $2 FOR UPDATE`, invitationID, accountID))
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("invitation not found")
		}
		if err != nil {
			return err
		}
		if inv.state(now) != accountsv1.InvitationState_INVITATION_STATE_PENDING {
			return failedPrecondition("the invitation is not pending")
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_account_invitations SET revoke_time = $2 WHERE invitation_id = $1`,
			invitationID, now); err != nil {
			return err
		}
		inv.RevokeTime = &now
		return h.recordAudit(ctx, tx, audit, now, auditEvent{
			BillingAccountID: accountID, Kind: AuditInvitationRevoked, ActorUserID: userID, InvitationID: invitationID,
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "revoke_invitation", err)
	}
	h.flushAudit(ctx, audit)
	return connect.NewResponse(&accountsv1.RevokeInvitationResponse{Invitation: inv.proto(now)}), nil
}

// AcceptInvitation adds the caller as a member with the invited role. The
// caller's Logto primary email must equal the invited address under ASCII
// case folding.
func (h *BillingAccountHandler) AcceptInvitation(ctx context.Context, req *connect.Request[accountsv1.AcceptInvitationRequest]) (*connect.Response[accountsv1.AcceptInvitationResponse], error) {
	userID, err := callerUserID(ctx)
	if err != nil {
		return nil, err
	}
	token := req.Msg.GetInvitationToken()
	if token == "" {
		return nil, invalidArgument("invitation_token is required")
	}
	if !wellFormedInvitationToken(token) {
		return nil, notFound("invitation not found")
	}
	hash := hashInvitationToken(token)
	early := h.timestamp()

	// Check the invitation before the Logto call, so that an unknown or
	// used token costs no Management API request. This check uses an early
	// timestamp; the transaction below repeats it with a fresh one.
	inv, err := scanInvitation(h.db.Pool.QueryRow(ctx, `SELECT `+invitationColumns+`
		FROM billing_account_invitations WHERE token_sha256 = $1`, hash))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, notFound("invitation not found")
	}
	if err != nil {
		return nil, h.internal(ctx, "accept_invitation", err)
	}
	if inv.state(early) != accountsv1.InvitationState_INVITATION_STATE_PENDING {
		return nil, failedPrecondition("the invitation is no longer valid")
	}

	user, err := h.directory.User(ctx, userID)
	switch {
	case errors.Is(err, directory.ErrUserNotFound):
		return nil, permissionDenied("the invitation was sent to another email address")
	case err != nil:
		h.logger.WarnContext(ctx, "logto_user_read_failed", slog.String("user_id", userID), slog.Any("error", err))
		return nil, unavailable("the account email address could not be read; try again later")
	}
	if user.PrimaryEmail == "" || emailKey(user.PrimaryEmail) != emailKey(inv.Email) {
		return nil, permissionDenied("the invitation was sent to another email address")
	}

	var account billingAccount
	var acceptTime time.Time
	audit := &auditLog{}
	err = h.db.InTx(ctx, func(tx pgx.Tx) error {
		// Lock the billing account first, as every other change does, then
		// read the invitation again under that lock.
		var name string
		var createTime time.Time
		err := tx.QueryRow(ctx, `SELECT name, create_time FROM billing_accounts
			WHERE billing_account_id = $1 AND delete_time IS NULL FOR UPDATE`, inv.BillingAccountID).Scan(&name, &createTime)
		if errors.Is(err, pgx.ErrNoRows) {
			// Deleting a billing account revokes its invitations.
			return failedPrecondition("the invitation is no longer valid")
		}
		if err != nil {
			return err
		}
		inv, err = scanInvitation(tx.QueryRow(ctx, `SELECT `+invitationColumns+`
			FROM billing_account_invitations WHERE invitation_id = $1`, inv.ID))
		if err != nil {
			return err
		}
		// Sample the clock after the Logto lookup and the lock wait, so that
		// an invitation that expired meanwhile is rejected and the acceptance
		// time is the time of the check. A retried transaction samples again.
		now := h.timestamp()
		if inv.state(now) != accountsv1.InvitationState_INVITATION_STATE_PENDING {
			return failedPrecondition("the invitation is no longer valid")
		}
		acceptTime = now
		var member bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_account_members
			WHERE billing_account_id = $1 AND user_id = $2)`, inv.BillingAccountID, userID).Scan(&member); err != nil {
			return err
		}
		if member {
			return alreadyExists("the caller is already a member of the billing account")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO billing_account_members (billing_account_id, user_id, role, create_time)
			VALUES ($1, $2, $3, $4)`, inv.BillingAccountID, userID, string(inv.Role), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE billing_account_invitations SET accept_time = $2, accepted_user_id = $3
			WHERE invitation_id = $1`, inv.ID, now, userID); err != nil {
			return err
		}
		account = billingAccount{ID: inv.BillingAccountID, Name: name, CreateTime: createTime, CallerRole: inv.Role}
		return h.recordAudit(ctx, tx, audit, now, auditEvent{
			BillingAccountID: inv.BillingAccountID, Kind: AuditInvitationAccepted, ActorUserID: userID,
			TargetUserID: userID, InvitationID: inv.ID, Details: map[string]string{"role": string(inv.Role)},
		})
	})
	if err != nil {
		return nil, h.internal(ctx, "accept_invitation", err)
	}
	h.flushAudit(ctx, audit)
	return connect.NewResponse(&accountsv1.AcceptInvitationResponse{
		BillingAccount: account.proto(),
		Member: &accountsv1.BillingAccountMember{
			BillingAccountId: account.ID,
			UserId:           userID,
			Role:             inv.Role.Proto(),
			DisplayName:      user.Name,
			CreateTime:       timestamppb.New(acceptTime),
		},
	}), nil
}
