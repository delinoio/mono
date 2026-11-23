// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
)

// Volume limits of CreateInvitation. Every invitation row counts toward both
// limits for invitationRateWindow after its creation, in every state: a
// revoked, expired, accepted or delivery-failed invitation still counts, so a
// revoke or a new billing account does not reset a limit.
//
// The values are a selected default. They are far above the invitation volume
// of a normal team and keep one user far below the Resend send limit that the
// billing emails share.
const (
	// invitationInviterLimit is the most invitations that one inviter can
	// create across all billing accounts in invitationRateWindow.
	invitationInviterLimit = 30
	// invitationEmailLimit is the most invitations that one email address can
	// receive, from any inviter and any billing account, in
	// invitationRateWindow.
	invitationEmailLimit = 5
	// invitationRateWindow is the time that an invitation counts toward the
	// limits.
	invitationRateWindow = 24 * time.Hour
)

// invitationRateMessage is the message of the resource_exhausted error. It
// names no limit kind, so the response does not tell a caller whether an
// address has received invitations from others.
const invitationRateMessage = "too many invitations; try again later"

// invitationLimitKind is the limit that a rejected CreateInvitation hit. The
// value is logged in the invitation_rate_limited record.
type invitationLimitKind string

const (
	invitationLimitInviter invitationLimitKind = "inviter"
	invitationLimitEmail   invitationLimitKind = "email"
)

// resourceExhausted returns a Connect resource_exhausted error. Like the other
// error helpers, its message never contains secrets or email addresses.
func resourceExhausted(msg string) error {
	return connect.NewError(connect.CodeResourceExhausted, errors.New(msg))
}

// enforceInvitationRate fails with resource_exhausted when one more invitation
// from userID to the address with emailKey would exceed a limit at now. It
// must run in the CreateInvitation transaction after the billing account lock
// and before the insert.
//
// Two transaction-scoped advisory locks serialize the count and the insert for
// one inviter and for one address, so concurrent calls cannot pass the same
// count and exceed a limit. The lock order is the billing account (taken by
// the caller), the inviter, then the address. The advisory locks release at
// the end of the transaction, after the new row is committed or rolled back.
// No other code path takes an advisory lock of these kinds, and no path takes
// them before the account lock, so the order cannot deadlock.
func (h *BillingAccountHandler) enforceInvitationRate(ctx context.Context, tx pgx.Tx, accountID, userID, emailKey string, now time.Time) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('invitation_rate_inviter:' || $1::text, 0))`, userID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('invitation_rate_email:' || $1::text, 0))`, emailKey); err != nil {
		return err
	}
	since := now.Add(-invitationRateWindow)
	checks := []struct {
		kind  invitationLimitKind
		limit int
		query string
		arg   string
	}{
		{invitationLimitInviter, invitationInviterLimit,
			`SELECT count(*) FROM billing_account_invitations WHERE inviter_user_id = $1 AND create_time > $2`, userID},
		{invitationLimitEmail, invitationEmailLimit,
			`SELECT count(*) FROM billing_account_invitations WHERE email_key = $1 AND create_time > $2`, emailKey},
	}
	for _, c := range checks {
		var n int
		if err := tx.QueryRow(ctx, c.query, c.arg, since).Scan(&n); err != nil {
			return err
		}
		if n >= c.limit {
			// The record names the limit kind and never the address.
			h.logger.WarnContext(ctx, "invitation_rate_limited",
				slog.String("billing_account_id", accountID), slog.String("inviter_user_id", userID),
				slog.String("limit_kind", string(c.kind)), slog.Int("limit", c.limit))
			return resourceExhausted(invitationRateMessage)
		}
	}
	return nil
}
