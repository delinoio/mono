// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditKind is the kind of a billing audit event. The values are stored, so
// they never change.
type AuditKind string

const (
	AuditBillingAccountCreated AuditKind = "billing_account_created"
	AuditBillingAccountRenamed AuditKind = "billing_account_renamed"
	AuditBillingAccountDeleted AuditKind = "billing_account_deleted"
	AuditInvitationCreated     AuditKind = "invitation_created"
	AuditInvitationRevoked     AuditKind = "invitation_revoked"
	AuditInvitationAccepted    AuditKind = "invitation_accepted"
	AuditMemberRoleChanged     AuditKind = "member_role_changed"
	AuditMemberRemoved         AuditKind = "member_removed"
	AuditOwnershipTransferred  AuditKind = "ownership_transferred"

	AuditSubscriptionStarted      AuditKind = "subscription_started"
	AuditSubscriptionCanceled     AuditKind = "subscription_canceled"
	AuditSubscriptionResumed      AuditKind = "subscription_resumed"
	AuditTopUpRequested           AuditKind = "top_up_requested"
	AuditAutoTopUpSettingsUpdated AuditKind = "auto_top_up_settings_updated"
	AuditAlertSettingsUpdated     AuditKind = "alert_settings_updated"
)

// auditEvent is one billing audit event. Details never hold secrets or email
// addresses.
type auditEvent struct {
	BillingAccountID string
	Kind             AuditKind
	ActorUserID      string
	TargetUserID     string
	InvitationID     string
	Details          map[string]string
}

// recordAudit inserts the event in tx and logs it.
func (s *Service) recordAudit(ctx context.Context, tx pgx.Tx, at time.Time, ev auditEvent) error {
	id, err := newID()
	if err != nil {
		return err
	}
	details := ev.Details
	if details == nil {
		details = map[string]string{}
	}
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_audit_events
		(audit_event_id, billing_account_id, kind, actor_user_id, target_user_id, invitation_id, details, create_time)
		VALUES ($1, $2, $3, $4, NULLIF($5, ''), NULLIF($6, '')::uuid, $7, $8)`,
		id, ev.BillingAccountID, string(ev.Kind), ev.ActorUserID, ev.TargetUserID, ev.InvitationID, body, at)
	if err != nil {
		return err
	}
	s.logger.LogAttrs(ctx, slog.LevelInfo, "billing_audit_event",
		slog.String("kind", string(ev.Kind)),
		slog.String("billing_account_id", ev.BillingAccountID),
		slog.String("actor_user_id", ev.ActorUserID),
		slog.String("target_user_id", ev.TargetUserID),
		slog.String("invitation_id", ev.InvitationID))
	return nil
}
