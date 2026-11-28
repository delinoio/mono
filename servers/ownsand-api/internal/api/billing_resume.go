// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// pendingBillingResume reads the durable command ID under the sandbox
// ordering lock. The agent result handler can clear the ID at any time.
func pendingBillingResume(ctx context.Context, conn *pgx.Conn, orgID, sandboxID string) (string, error) {
	var commandID string
	err := conn.QueryRow(ctx, `SELECT COALESCE(billing_resume_command_id::text, '')
		FROM sandboxes WHERE organization_id = $1 AND sandbox_id = $2`, orgID, sandboxID).Scan(&commandID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errNotFound("sandbox")
	}
	return commandID, err
}

// enqueueBillingResume commits the command ID before sending to the agent.
// A crash before queueing also leaves the attempt unresolved until the agent
// fences it. Only a definite queue rejection allows immediate cleanup.
func (s *Service) enqueueBillingResume(ctx context.Context, conn *pgx.Conn, t billingTarget, commandID string, send func() error) error {
	tag, err := conn.Exec(ctx, `UPDATE sandboxes s SET billing_resume_command_id = $2
		FROM organization_billing b WHERE b.organization_id = s.organization_id
		AND s.sandbox_id = $1 AND s.paused_by_billing AND s.agent_state = $3 AND NOT s.delete_pending
		AND s.billing_resume_command_id IS NULL AND `+allowedPhaseSQL,
		t.sandboxID, commandID, int16(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED))
	if err != nil {
		return s.internal(ctx, "record billing resume", err)
	}
	if tag.RowsAffected() == 0 {
		return errBillingStale
	}
	if err := send(); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), callerPauseFinishTimeout)
		defer cancel()
		if _, cleanupErr := conn.Exec(cleanupCtx, `UPDATE sandboxes SET billing_resume_command_id = NULL
			WHERE sandbox_id = $1 AND billing_resume_command_id = $2`, t.sandboxID, commandID); cleanupErr != nil {
			s.Logger.WarnContext(cleanupCtx, "billing_resume_queue_rejection_cleanup_failed",
				slog.String("sandbox_id", t.sandboxID), slog.String("command_id", commandID), slog.Any("error", cleanupErr))
		}
		return err
	}
	return nil
}

type resumeFenceSupportKey struct{}

// sendResumeFence requests completion evidence and returns after queueing. The
// billing pass never holds the ordering lock while it waits for a fence reply.
func (s *Service) sendResumeFence(ctx context.Context, conn *hub.Conn, t billingTarget, resumeID string) error {
	if conn.Value(resumeFenceSupportKey{}) != true {
		return nil
	}
	_, err := s.sendCommand(ctx, conn, commandInfo{kind: cmdFenceResume, orgID: t.orgID, sandboxID: t.sandboxID},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_FenceResume{FenceResume: &ownsandv1.FenceResumeCommand{
			SandboxId: t.sandboxID, ResumeCommandId: resumeID,
		}}})
	return err
}

// settleBillingResume accepts only an entry for the sandbox and device that
// own the command ID. It applies the revision and clears the ID in one
// transaction, so a caller who sees no pending resume also sees its state.
// The channel's process-local command cache is unnecessary for either a
// matching resume result or a fence reply.
func (s *Service) settleBillingResume(ctx context.Context, conn *hub.Conn, commandID string, entry *ownsandv1.SandboxStateEntry) (bool, error) {
	if entry == nil || !uuidv7.Valid(commandID) || !uuidv7.Valid(entry.GetSandboxId()) {
		return false, nil
	}
	sandboxID := canonicalUUID(entry.GetSandboxId())
	var matched bool
	var applied entryResult
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		var owner string
		err := tx.QueryRow(ctx, `SELECT sandbox_id::text FROM sandboxes
			WHERE billing_resume_command_id = $1 AND device_id = $2 FOR UPDATE`, commandID, conn.DeviceID).Scan(&owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		matched = true
		if owner != sandboxID {
			return nil
		}
		applied, err = s.applySandboxEntry(ctx, tx, conn.DeviceID, entry, sandboxEntryFromCommandResult)
		if err != nil || !applied.known {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE sandboxes SET billing_resume_command_id = NULL WHERE sandbox_id = $1`, owner)
		return err
	})
	if err != nil {
		return matched, err
	}
	if applied.applied {
		s.watch.fire(sandboxID)
	}
	if applied.delete != nil {
		s.sendDelete(ctx, conn, conn.OrganizationID, *applied.delete)
	}
	return matched, nil
}
