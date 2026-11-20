// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// ControlEventOrganizationDeleted ends every session and relay grant of the
// organization and closes the channels of its devices after their clean-up
// command.
const ControlEventOrganizationDeleted ControlEventKind = 100

// DeleteOrganization deletes the organization in one transaction: it writes
// the ORGANIZATION_DELETED audit event, removes every device (ending its
// sandboxes with ORGANIZATION_DELETED and keeping the removal tombstones),
// records the R2 clean-up job of the organization prefix, writes the
// revocation and removal control events and deletes the organization's
// records. API keys, enrollment tokens and invitations are deleted with the
// organization row, which revokes them. Audit events, tombstones and the
// clean-up job state stay.
func (s *Store) DeleteOrganization(ctx context.Context, organizationID string, actor Actor, requestID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var deleted *time.Time
		err := tx.QueryRow(ctx, `SELECT deleted_at FROM organizations WHERE organization_id = $1 FOR UPDATE`, organizationID).Scan(&deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if deleted != nil {
			return ErrNotFound
		}
		if _, err := tx.Exec(ctx, `UPDATE organizations SET deleted_at = $2 WHERE organization_id = $1`, organizationID, s.now()); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT device_id FROM devices WHERE organization_id = $1 AND state <> $2 FOR UPDATE`,
			organizationID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
		if err != nil {
			return err
		}
		devices, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range devices {
			if err := s.removeDeviceTx(ctx, tx, organizationID, id, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_ORGANIZATION_DELETED, actor, requestID); err != nil {
				return err
			}
		}
		// Sandboxes of devices that were already removed are terminal; any
		// left over (none expected) end here too, so every sandbox gets its
		// single SANDBOX_DELETED event before the records go.
		rows, err = tx.Query(ctx, `SELECT sandbox_id FROM sandboxes WHERE organization_id = $1 AND agent_state NOT IN ($2, $3)`,
			organizationID, int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED), int16(ownsandv1.SandboxState_SANDBOX_STATE_FAILED))
		if err != nil {
			return err
		}
		rest, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range rest {
			if _, err := s.endSandboxTx(ctx, tx, organizationID, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_ORGANIZATION_DELETED, actor, requestID); err != nil {
				return err
			}
		}
		if err := s.cleanupJobTx(ctx, tx, organizationID, OrganizationPrefix(organizationID), ""); err != nil {
			return err
		}
		// Per-snapshot jobs are covered by the organization prefix job.
		if _, err := tx.Exec(ctx, `DELETE FROM r2_cleanup_jobs WHERE organization_id = $1 AND prefix <> $2`, organizationID, OrganizationPrefix(organizationID)); err != nil {
			return err
		}
		if err := s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventOrganizationDeleted, OrganizationID: organizationID}); err != nil {
			return err
		}
		if err := s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ORGANIZATION_DELETED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ORGANIZATION, TargetID: organizationID, RequestID: requestID,
		}); err != nil {
			return err
		}
		// Sandboxes, snapshots, templates, devices, memberships, invitations,
		// API keys, enrollment tokens and the policy cascade.
		if _, err := tx.Exec(ctx, `DELETE FROM sandboxes WHERE organization_id = $1`, organizationID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM signed_upload_issues WHERE organization_id = $1 AND last_expires_at < $2`, organizationID, s.now()); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM organizations WHERE organization_id = $1`, organizationID)
		return err
	})
}

// AgentReleases holds the releases of the signed release manifest.
type AgentRelease struct {
	Version   string
	Platform  string
	IsDefault bool
	Revoked   bool
}

// AgentVersionPolicy is the organization policy with the default targets.
type AgentVersionPolicy struct {
	OrganizationID  string
	Mode            ownsandv1.AgentVersionPolicyMode
	PinnedVersion   string
	DefaultVersions map[string]string
}

// ErrRevokedVersion is returned for a pin to a revoked version.
var ErrRevokedVersion = errors.New("the pinned version is revoked")

// GetAgentVersionPolicy returns the organization policy and the default
// target versions per platform.
func (s *Store) GetAgentVersionPolicy(ctx context.Context, organizationID string) (AgentVersionPolicy, error) {
	p := AgentVersionPolicy{OrganizationID: organizationID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT}
	var mode int16
	err := s.Pool.QueryRow(ctx, `SELECT mode, pinned_version FROM agent_version_policies WHERE organization_id = $1`, organizationID).Scan(&mode, &p.PinnedVersion)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return p, err
	}
	if err == nil {
		p.Mode = ownsandv1.AgentVersionPolicyMode(mode)
	}
	p.DefaultVersions, err = s.DefaultAgentVersions(ctx)
	return p, err
}

// DefaultAgentVersions returns the default target version per platform.
func (s *Store) DefaultAgentVersions(ctx context.Context) (map[string]string, error) {
	out := map[string]string{}
	rows, err := s.Pool.Query(ctx, `SELECT platform, version FROM agent_releases WHERE is_default AND NOT revoked`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var platform, version string
		if err := rows.Scan(&platform, &version); err != nil {
			return nil, err
		}
		out[platform] = version
	}
	return out, rows.Err()
}

// VersionRevoked reports whether a version is revoked on any platform.
func (s *Store) VersionRevoked(ctx context.Context, version string) (bool, error) {
	var revoked bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_releases WHERE version = $1 AND revoked)`, version).Scan(&revoked)
	return revoked, err
}

// UpdateAgentVersionPolicy stores the policy and writes the configuration
// event. A pin to a revoked version fails with ErrRevokedVersion.
func (s *Store) UpdateAgentVersionPolicy(ctx context.Context, organizationID string, mode ownsandv1.AgentVersionPolicyMode, pinned string, actor Actor, requestID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if mode == ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN {
			var revoked bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agent_releases WHERE version = $1 AND revoked)`, pinned).Scan(&revoked); err != nil {
				return err
			}
			if revoked {
				return ErrRevokedVersion
			}
		} else {
			pinned = ""
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_version_policies (organization_id, mode, pinned_version, updated_at) VALUES ($1, $2, $3, $4)
			ON CONFLICT (organization_id) DO UPDATE SET mode = EXCLUDED.mode, pinned_version = EXCLUDED.pinned_version, updated_at = EXCLUDED.updated_at`,
			organizationID, int16(mode), pinned, s.now()); err != nil {
			return err
		}
		if err := s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventAgentPolicyChanged, OrganizationID: organizationID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_AGENT_VERSION_POLICY_CHANGED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_AGENT_VERSION_POLICY, TargetID: organizationID, RequestID: requestID,
		})
	})
}

// ReplaceAgentReleases replaces the releases table with the releases of a
// verified manifest.
func (s *Store) ReplaceAgentReleases(ctx context.Context, releases []AgentRelease) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `DELETE FROM agent_releases`); err != nil {
			return err
		}
		for _, r := range releases {
			if _, err := tx.Exec(ctx, `INSERT INTO agent_releases (version, platform, is_default, revoked, updated_at) VALUES ($1, $2, $3, $4, $5)`,
				r.Version, r.Platform, r.IsDefault, r.Revoked, s.now()); err != nil {
				return err
			}
		}
		return nil
	})
}
