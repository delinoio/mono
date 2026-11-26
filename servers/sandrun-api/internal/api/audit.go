// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
	"github.com/delinoio/mono/protos/delino/sandrun/v1/sandrunv1connect"
	"github.com/delinoio/mono/servers/sandrun-api/internal/auth"
	"github.com/delinoio/mono/servers/sandrun-api/internal/authz"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

// AuditEvent is one audit record. It never holds credentials, command
// arguments, environment values, command output or file contents.
type AuditEvent struct {
	OrganizationID string
	Action         sandrunv1.AuditAction
	// Actor is the acting principal; the zero value is SandRun itself.
	Actor      auth.Principal
	TargetKind sandrunv1.AuditTargetKind
	TargetID   string
	// SubjectUserID is the dev session owner for dev session events and the
	// affected member for membership and API key events.
	SubjectUserID string
}

// WriteAudit inserts an audit event. Call it in the transaction of the
// action, so a rejected or failed action writes nothing.
func (s *Service) WriteAudit(ctx context.Context, q db.Querier, e AuditEvent) error {
	var actorKey any
	if e.Actor.APIKeyID != "" {
		actorKey = e.Actor.APIKeyID
	}
	_, err := q.Exec(ctx, `INSERT INTO audit_events (audit_event_id, organization_id, action, actor_user_id, actor_api_key_id, target_kind, target_id, subject_user_id, rpc_id, create_time)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		NewID(), e.OrganizationID, int16(e.Action), e.Actor.UserID, actorKey, int16(e.TargetKind), e.TargetID, e.SubjectUserID, RPCID(ctx), s.NowUTC())
	return err
}

// AuditHandler serves AuditService.
type AuditHandler struct{ *Service }

var _ sandrunv1connect.AuditServiceHandler = AuditHandler{}

// ListAuditEvents lists the events of the organization, newest first.
func (h AuditHandler) ListAuditEvents(ctx context.Context, req *connect.Request[sandrunv1.ListAuditEventsRequest]) (*connect.Response[sandrunv1.ListAuditEventsResponse], error) {
	c, err := h.Authorize(ctx, h.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionRead)
	if err != nil {
		return nil, err
	}
	limit, after, err := PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	var q db.Querier = h.DB.Pool
	query := `SELECT audit_event_id::text, action, actor_user_id, COALESCE(actor_api_key_id::text, ''), target_kind, target_id, subject_user_id, create_time
		FROM audit_events WHERE organization_id = $1`
	args := []any{c.OrganizationID, limit}
	if after != "" {
		// Read the anchor and the page from the same snapshot so retention
		// cannot remove the anchor between the two reads.
		tx, err := h.DB.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, h.Internal(ctx, "begin audit events page", err)
		}
		defer tx.Rollback(ctx)

		var anchorTime time.Time
		err = tx.QueryRow(ctx, `SELECT create_time FROM audit_events
			WHERE organization_id = $1 AND audit_event_id = $2`, c.OrganizationID, after).Scan(&anchorTime)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidf("invalid page_token")
		}
		if err != nil {
			return nil, h.Internal(ctx, "find audit events page anchor", err)
		}
		q = tx
		query += " AND (create_time, audit_event_id) < ($3, $4)"
		args = append(args, anchorTime, after)
	}
	rows, err := q.Query(ctx, query+" ORDER BY create_time DESC, audit_event_id DESC LIMIT $2", args...)
	if err != nil {
		return nil, h.Internal(ctx, "list audit events", err)
	}
	defer rows.Close()
	resp := &sandrunv1.ListAuditEventsResponse{}
	last := ""
	for rows.Next() {
		e := &sandrunv1.AuditEvent{OrganizationId: c.OrganizationID}
		var action, kind int16
		var created time.Time
		if err := rows.Scan(&e.AuditEventId, &action, &e.ActorUserId, &e.ActorApiKeyId, &kind, &e.TargetId, &e.SubjectUserId, &created); err != nil {
			return nil, h.Internal(ctx, "list audit events", err)
		}
		e.Action = sandrunv1.AuditAction(action)
		e.TargetKind = sandrunv1.AuditTargetKind(kind)
		e.CreateTime = Timestamp(created)
		resp.Events = append(resp.Events, e)
		last = e.AuditEventId
	}
	if err := rows.Err(); err != nil {
		return nil, h.Internal(ctx, "list audit events", err)
	}
	resp.NextPageToken = NextPageToken(len(resp.Events), limit, last)
	return connect.NewResponse(resp), nil
}
