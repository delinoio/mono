// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// auditEvent is one audit record. It never holds exec commands, exec
// output, file contents, secrets or labels.
type auditEvent struct {
	OrganizationID string
	Action         ownsandv1.AuditAction
	Actor          auth.Principal
	EndReason      ownsandv1.SandboxEndReason
	TargetKind     ownsandv1.AuditTargetKind
	TargetID       string
}

// writeAudit inserts an audit event. Call it in the transaction of the
// action, so a rejected or failed action writes nothing.
func (s *Service) writeAudit(ctx context.Context, q db.Querier, e auditEvent) error {
	var endReason any
	if e.EndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
		endReason = int16(e.EndReason)
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_events (audit_event_id, organization_id, action, actor_kind, actor_id, end_reason, target_kind, target_id, request_id, create_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		newID(), e.OrganizationID, int16(e.Action), int16(e.Actor.Kind), e.Actor.ID, endReason, int16(e.TargetKind), e.TargetID, requestID(ctx), s.now())
	return err
}

func durationProto(d time.Duration) *durationpb.Duration {
	if d <= 0 {
		return nil
	}
	return durationpb.New(d)
}
