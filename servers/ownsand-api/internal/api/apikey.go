// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// APIKeyHandler serves ApiKeyService.
type APIKeyHandler struct{ *Service }

var _ ownsandv1connect.ApiKeyServiceHandler = APIKeyHandler{}

const apiKeyColumns = `api_key_id::text, organization_id::text, name, key_prefix, created_by_kind, created_by_id, create_time, revoke_time`

func scanAPIKey(row pgx.Row) (*ownsandv1.ApiKey, error) {
	var k ownsandv1.ApiKey
	var kind int16
	var createdBy string
	var created time.Time
	var revoked *time.Time
	if err := row.Scan(&k.ApiKeyId, &k.OrganizationId, &k.Name, &k.KeyPrefix, &kind, &createdBy, &created, &revoked); err != nil {
		return nil, err
	}
	k.CreatedBy = principalProto(kind, createdBy)
	k.CreateTime = ts(created)
	k.RevokeTime = tsPtr(revoked)
	k.State = ownsandv1.ApiKeyState_API_KEY_STATE_ACTIVE
	if revoked != nil {
		k.State = ownsandv1.ApiKeyState_API_KEY_STATE_REVOKED
	}
	return &k, nil
}

func (h APIKeyHandler) CreateApiKey(ctx context.Context, req *connect.Request[ownsandv1.CreateApiKeyRequest]) (*connect.Response[ownsandv1.CreateApiKeyResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	var resp *ownsandv1.CreateApiKeyResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageAPIKeys)
		if err != nil {
			return err
		}
		if err := validateName("name", req.Msg.GetName()); err != nil {
			return err
		}
		secret, err := auth.NewSecret(auth.SecretAPIKey)
		if err != nil {
			return err
		}
		id := newID()
		key, err := scanAPIKey(tx.QueryRow(ctx, `INSERT INTO api_keys (api_key_id, organization_id, name, secret_sha256, key_prefix, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+apiKeyColumns,
			id, orgID, req.Msg.GetName(), secret.Hash, secret.DisplayPrefix, int16(p.Kind), p.ID, h.now()))
		if err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_API_KEY_CREATED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: id,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.CreateApiKeyResponse{ApiKey: key, ApiKeySecret: secret.Value}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "create api key", err)
	}
	return connect.NewResponse(resp), nil
}

func (h APIKeyHandler) ListApiKeys(ctx context.Context, req *connect.Request[ownsandv1.ListApiKeysRequest]) (*connect.Response[ownsandv1.ListApiKeysResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionManageAPIKeys); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT `+apiKeyColumns+` FROM api_keys
		WHERE organization_id = $1 AND api_key_id > $2 ORDER BY api_key_id LIMIT $3`, orgID, afterKey(after), limit)
	if err != nil {
		return nil, h.internal(ctx, "list api keys", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListApiKeysResponse{}
	for rows.Next() {
		k, err := scanAPIKey(rows)
		if err != nil {
			return nil, h.internal(ctx, "list api keys", err)
		}
		resp.ApiKeys = append(resp.ApiKeys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list api keys", err)
	}
	last := ""
	if n := len(resp.ApiKeys); n > 0 {
		last = resp.ApiKeys[n-1].GetApiKeyId()
	}
	resp.NextPageToken = nextPageToken(len(resp.ApiKeys), limit, last)
	return connect.NewResponse(resp), nil
}

func (h APIKeyHandler) RevokeApiKey(ctx context.Context, req *connect.Request[ownsandv1.RevokeApiKeyRequest]) (*connect.Response[ownsandv1.RevokeApiKeyResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	rawKeyID := req.Msg.GetApiKeyId()
	var resp *ownsandv1.RevokeApiKeyResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageAPIKeys)
		if err != nil {
			return err
		}
		keyID, err := normalizeID("api_key_id", rawKeyID)
		if err != nil {
			return err
		}
		key, err := scanAPIKey(tx.QueryRow(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE organization_id = $1 AND api_key_id = $2 FOR UPDATE", orgID, keyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("API key")
		}
		if err != nil {
			return err
		}
		if key.GetState() == ownsandv1.ApiKeyState_API_KEY_STATE_REVOKED {
			// Revoking a revoked key succeeds without a change or audit event.
			resp = &ownsandv1.RevokeApiKeyResponse{ApiKey: key}
			return nil
		}
		key, err = scanAPIKey(tx.QueryRow(ctx, "UPDATE api_keys SET revoke_time = $3 WHERE organization_id = $1 AND api_key_id = $2 RETURNING "+apiKeyColumns, orgID, keyID, h.now()))
		if err != nil {
			return err
		}
		if err := events.Write(ctx, tx, events.Event{Kind: events.KindAPIKeyRevoked, OrganizationID: orgID, APIKeyID: keyID}); err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: keyID,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.RevokeApiKeyResponse{ApiKey: key}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "revoke api key", err)
	}
	h.notify()
	return connect.NewResponse(resp), nil
}
