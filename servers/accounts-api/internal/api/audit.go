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

	AuditOrganizationLinked      AuditKind = "organization_linked"
	AuditOrganizationLinkRenamed AuditKind = "organization_link_renamed"
	AuditOrganizationUnlinked    AuditKind = "organization_unlinked"
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

// auditLog collects the billing audit events that a handler inserted in a
// transaction, so that their log records are written only after that
// transaction committed. A failed or rolled-back call then logs no
// billing_audit_event record for a row that does not exist. The zero value is
// ready to use. A collector belongs to one call and is never shared.
type auditLog struct {
	events []auditEvent
}

// recordAudit inserts the event in tx and adds it to audit. The log record is
// written by flushAudit once the transaction has committed.
func (s *Service) recordAudit(ctx context.Context, tx pgx.Tx, audit *auditLog, at time.Time, ev auditEvent) error {
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
	audit.events = append(audit.events, ev)
	return nil
}

// flushAudit logs the events that audit collected and empties it. Call it
// only after the transaction that inserted the events committed.
func (s *Service) flushAudit(ctx context.Context, audit *auditLog) {
	for _, ev := range audit.events {
		s.logger.LogAttrs(ctx, slog.LevelInfo, "billing_audit_event",
			slog.String("kind", string(ev.Kind)),
			slog.String("billing_account_id", ev.BillingAccountID),
			slog.String("actor_user_id", ev.ActorUserID),
			slog.String("target_user_id", ev.TargetUserID),
			slog.String("invitation_id", ev.InvitationID))
	}
	audit.events = nil
}
