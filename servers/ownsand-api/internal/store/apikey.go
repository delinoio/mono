// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
)

// APIKey is an API key row without its hash.
type APIKey struct {
	ID             string
	OrganizationID string
	Name           string
	SecretPrefix   string
	CreatedBy      Actor
	CreatedAt      time.Time
	RevokedAt      *time.Time
}

// CreateAPIKey stores an API key hash.
func (s *Store) CreateAPIKey(ctx context.Context, organizationID, name string, actor Actor, secretHash []byte, secretPrefix, requestID string) (APIKey, error) {
	k := APIKey{ID: NewID(), OrganizationID: organizationID, Name: name, SecretPrefix: secretPrefix, CreatedBy: actor, CreatedAt: s.now()}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO api_keys (api_key_id, organization_id, name, secret_hash, secret_prefix, created_by_kind, created_by_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, k.ID, organizationID, name, secretHash, secretPrefix, int16(actor.Kind), actor.ID, k.CreatedAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_API_KEY_CREATED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: k.ID, RequestID: requestID,
		})
	})
	return k, err
}

// ListAPIKeys lists the organization's API keys.
func (s *Store) ListAPIKeys(ctx context.Context, organizationID string, p Page) ([]APIKey, error) {
	rows, err := s.Pool.Query(ctx, `SELECT api_key_id, organization_id, name, secret_prefix, created_by_kind, created_by_id, created_at, revoked_at
		FROM api_keys WHERE organization_id = $1 AND api_key_id > $2 ORDER BY api_key_id LIMIT $3`, organizationID, afterArg(p), p.Size)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (APIKey, error) {
		var k APIKey
		var kind int16
		err := r.Scan(&k.ID, &k.OrganizationID, &k.Name, &k.SecretPrefix, &kind, &k.CreatedBy.ID, &k.CreatedAt, &k.RevokedAt)
		k.CreatedBy.Kind = ownsandv1.PrincipalKind(kind)
		return k, err
	})
}

// RevokeAPIKey revokes an API key and writes the revocation event of its
// sessions and relay grants. Revoking a revoked key succeeds without change.
func (s *Store) RevokeAPIKey(ctx context.Context, organizationID, apiKeyID string, actor Actor, requestID string) error {
	if !ValidID(apiKeyID) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var revoked *time.Time
		err := tx.QueryRow(ctx, `SELECT revoked_at FROM api_keys WHERE organization_id = $1 AND api_key_id = $2 FOR UPDATE`, organizationID, apiKeyID).Scan(&revoked)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || revoked != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE api_keys SET revoked_at = $2 WHERE api_key_id = $1`, apiKeyID, s.now()); err != nil {
			return err
		}
		if err := s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventAPIKeyRevoked, OrganizationID: organizationID, APIKeyID: apiKeyID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_API_KEY_REVOKED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_API_KEY, TargetID: apiKeyID, RequestID: requestID,
		})
	})
}

// LookupAPIKey implements auth.CredentialStore.
func (s *Store) LookupAPIKey(ctx context.Context, hash []byte) (auth.APIKeyRecord, error) {
	var rec auth.APIKeyRecord
	var revoked *time.Time
	var deleted *time.Time
	err := s.Pool.QueryRow(ctx, `SELECT k.api_key_id, k.organization_id, k.secret_hash, k.revoked_at, o.deleted_at
		FROM api_keys k JOIN organizations o USING (organization_id) WHERE k.secret_hash = $1`, hash).
		Scan(&rec.APIKeyID, &rec.OrganizationID, &rec.SecretHash, &revoked, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, auth.ErrNotFound
	}
	rec.Revoked = revoked != nil
	rec.OrgDeleted = deleted != nil
	return rec, err
}

// LookupDeviceSession implements auth.CredentialStore.
func (s *Store) LookupDeviceSession(ctx context.Context, hash []byte) (auth.DeviceSessionRecord, error) {
	var rec auth.DeviceSessionRecord
	var state int16
	err := s.Pool.QueryRow(ctx, `SELECT d.device_id, d.organization_id, ds.credential_hash, ds.expires_at, d.state
		FROM device_sessions ds JOIN devices d USING (device_id) WHERE ds.credential_hash = $1`, hash).
		Scan(&rec.DeviceID, &rec.OrganizationID, &rec.CredentialHash, &rec.ExpiresAt, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, auth.ErrNotFound
	}
	rec.DeviceRemoved = ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED
	return rec, err
}
