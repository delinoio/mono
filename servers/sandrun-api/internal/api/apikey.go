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
)

// APIKeyHandler serves ApiKeyService. Callers manage only their own
// personal API keys. The authentication interceptor already rejects a
// personal API key on these RPCs, and ActionManageOwnAPIKeys rejects it
// again.
type APIKeyHandler struct{ *Service }

var _ sandrunv1connect.ApiKeyServiceHandler = APIKeyHandler{}

const apiKeyColumns = `api_key_id::text, organization_id::text, name, display_prefix, create_time, expire_time, last_used_time, revoke_time`

func (s *Service) scanAPIKey(row pgx.Row) (*sandrunv1.ApiKey, error) {
	var k sandrunv1.ApiKey
	var created time.Time
	var expires, lastUsed, revoked *time.Time
	if err := row.Scan(&k.ApiKeyId, &k.OrganizationId, &k.Name, &k.DisplayPrefix, &created, &expires, &lastUsed, &revoked); err != nil {
		return nil, err
	}
	k.CreatedAt = Timestamp(created)
	k.ExpiresAt = TimestampPtr(expires)
	k.LastUsedAt = TimestampPtr(lastUsed)
	k.RevokedAt = TimestampPtr(revoked)
	k.State = apiKeyState(s.NowUTC(), expires, revoked)
	return &k, nil
}

// apiKeyState returns the state of a key: a revoked key is REVOKED, a key
// whose expiry has passed is EXPIRED, any other key is ACTIVE.
func apiKeyState(now time.Time, expires, revoked *time.Time) sandrunv1.ApiKeyState {
	switch {
	case revoked != nil:
		return sandrunv1.ApiKeyState_API_KEY_STATE_REVOKED
	case expires != nil && !now.Before(*expires):
		return sandrunv1.ApiKeyState_API_KEY_STATE_EXPIRED
	default:
		return sandrunv1.ApiKeyState_API_KEY_STATE_ACTIVE
	}
}

func (h APIKeyHandler) CreateApiKey(ctx context.Context, req *connect.Request[sandrunv1.CreateApiKeyRequest]) (*connect.Response[sandrunv1.CreateApiKeyResponse], error) {
	var resp *sandrunv1.CreateApiKeyResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageOwnAPIKeys)
		if err != nil {
			return err
		}
		if err := ValidateName("name", req.Msg.GetName(), maxAPIKeyNameLength); err != nil {
			return err
		}
		var expires *time.Time
		if ts := req.Msg.GetExpiresAt(); ts != nil {
			if err := ts.CheckValid(); err != nil {
				return ErrInvalidf("invalid expires_at: %v", err)
			}
			t := ts.AsTime().UTC()
			if !t.After(h.NowUTC()) {
				return ErrInvalidf("invalid expires_at: must be in the future")
			}
			expires = &t
		}
		if err := LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		// Lock the caller's membership after the organization, the lock
		// order of RemoveMember. A removal that committed after Authorize
		// deleted the row, so the key is not created; a removal that starts
		// later waits for this commit and then revokes the new key too.
		var locked int
		err = tx.QueryRow(ctx, "SELECT 1 FROM memberships WHERE organization_id = $1 AND user_id = $2 FOR KEY SHARE", c.OrganizationID, c.UserID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound("organization")
		}
		if err != nil {
			return err
		}
		now := h.NowUTC()
		if expires != nil && !expires.After(now) {
			return ErrInvalidf("invalid expires_at: must be in the future")
		}
		secret, err := auth.NewSecret(auth.SecretAPIKey)
		if err != nil {
			return err
		}
		id := NewID()
		key, err := h.scanAPIKey(tx.QueryRow(ctx, `INSERT INTO api_keys (api_key_id, organization_id, user_id, name, secret_sha256, display_prefix, create_time, expire_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+apiKeyColumns,
			id, c.OrganizationID, c.UserID, req.Msg.GetName(), secret.Hash, secret.DisplayPrefix, now, expires))
		if err != nil {
			return err
		}
		if err := h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: sandrunv1.AuditAction_AUDIT_ACTION_API_KEY_CREATED, Actor: c.Principal,
			TargetKind: sandrunv1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: id, SubjectUserID: c.UserID,
		}); err != nil {
			return err
		}
		resp = &sandrunv1.CreateApiKeyResponse{ApiKey: key, Secret: secret.Value}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "create api key", err)
	}
	return connect.NewResponse(resp), nil
}

func (h APIKeyHandler) ListApiKeys(ctx context.Context, req *connect.Request[sandrunv1.ListApiKeysRequest]) (*connect.Response[sandrunv1.ListApiKeysResponse], error) {
	c, err := h.Authorize(ctx, h.DB.Pool, req.Msg.GetOrganizationId(), authz.ActionManageOwnAPIKeys)
	if err != nil {
		return nil, err
	}
	limit, after, err := PageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT `+apiKeyColumns+` FROM api_keys
		WHERE organization_id = $1 AND user_id = $2 AND api_key_id > $3 ORDER BY api_key_id LIMIT $4`, c.OrganizationID, c.UserID, AfterKey(after), limit)
	if err != nil {
		return nil, h.Internal(ctx, "list api keys", err)
	}
	defer rows.Close()
	resp := &sandrunv1.ListApiKeysResponse{}
	for rows.Next() {
		k, err := h.scanAPIKey(rows)
		if err != nil {
			return nil, h.Internal(ctx, "list api keys", err)
		}
		resp.ApiKeys = append(resp.ApiKeys, k)
	}
	if err := rows.Err(); err != nil {
		return nil, h.Internal(ctx, "list api keys", err)
	}
	last := ""
	if n := len(resp.ApiKeys); n > 0 {
		last = resp.ApiKeys[n-1].GetApiKeyId()
	}
	resp.NextPageToken = NextPageToken(len(resp.ApiKeys), limit, last)
	return connect.NewResponse(resp), nil
}

func (h APIKeyHandler) RevokeApiKey(ctx context.Context, req *connect.Request[sandrunv1.RevokeApiKeyRequest]) (*connect.Response[sandrunv1.RevokeApiKeyResponse], error) {
	var resp *sandrunv1.RevokeApiKeyResponse
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		c, err := h.Authorize(ctx, tx, req.Msg.GetOrganizationId(), authz.ActionManageOwnAPIKeys)
		if err != nil {
			return err
		}
		keyID, err := NormalizeID("api_key_id", req.Msg.GetApiKeyId())
		if err != nil {
			return err
		}
		if err := LockOrganizationShare(ctx, tx, c.OrganizationID); err != nil {
			return err
		}
		// A key of another user is not found: each user sees only their own
		// keys.
		key, err := h.scanAPIKey(tx.QueryRow(ctx, "SELECT "+apiKeyColumns+" FROM api_keys WHERE organization_id = $1 AND user_id = $2 AND api_key_id = $3 FOR UPDATE",
			c.OrganizationID, c.UserID, keyID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound("API key")
		}
		if err != nil {
			return err
		}
		if key.GetState() == sandrunv1.ApiKeyState_API_KEY_STATE_REVOKED {
			// Revoking a revoked key succeeds without a change or audit event.
			resp = &sandrunv1.RevokeApiKeyResponse{ApiKey: key}
			return nil
		}
		key, err = h.scanAPIKey(tx.QueryRow(ctx, "UPDATE api_keys SET revoke_time = $2 WHERE api_key_id = $1 RETURNING "+apiKeyColumns, keyID, h.NowUTC()))
		if err != nil {
			return err
		}
		if err := h.WriteAudit(ctx, tx, AuditEvent{
			OrganizationID: c.OrganizationID, Action: sandrunv1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED, Actor: c.Principal,
			TargetKind: sandrunv1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: keyID, SubjectUserID: c.UserID,
		}); err != nil {
			return err
		}
		resp = &sandrunv1.RevokeApiKeyResponse{ApiKey: key}
		return nil
	})
	if err != nil {
		return nil, h.Internal(ctx, "revoke api key", err)
	}
	return connect.NewResponse(resp), nil
}
