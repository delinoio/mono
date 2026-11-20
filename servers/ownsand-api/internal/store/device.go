// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"bytes"
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Tombstone lifetime after a device removal commits.
const TombstoneLifetime = 365 * 24 * time.Hour

// EnrollmentToken is an enrollment token row without its hash.
type EnrollmentToken struct {
	ID             string
	OrganizationID string
	DeviceName     string
	SecretPrefix   string
	CreatedBy      Actor
	CreatedAt      time.Time
	ExpiresAt      time.Time
	RevokedAt      *time.Time
	UsedAt         *time.Time
	DeviceID       string
}

// State derives the token state at now.
func (t EnrollmentToken) State(now time.Time) ownsandv1.EnrollmentTokenState {
	switch {
	case t.UsedAt != nil:
		return ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_USED
	case t.RevokedAt != nil:
		return ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_REVOKED
	case !now.Before(t.ExpiresAt):
		return ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_EXPIRED
	default:
		return ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_PENDING
	}
}

// Limits are device limits or sandbox resources.
type Limits struct {
	VCPUCount int64
	MemoryMiB int64
	DiskGiB   int64
}

// Device is a device row with its allocation.
type Device struct {
	ID               string
	OrganizationID   string
	Name             string
	State            ownsandv1.DeviceState
	PublicKey        []byte
	Capabilities     *ownsandv1.DeviceCapabilities
	Limits           Limits
	Allocated        Limits
	AgentVersion     string
	UpdateState      ownsandv1.AgentUpdateState
	PendingIdleSince *time.Time
	StateEpoch       string
	EnrolledAt       time.Time
	LastSeenAt       *time.Time
	RemovedAt        *time.Time
}

// CreateEnrollmentToken stores an enrollment token hash.
func (s *Store) CreateEnrollmentToken(ctx context.Context, organizationID, deviceName string, ttl time.Duration, actor Actor, secretHash []byte, secretPrefix, requestID string) (EnrollmentToken, error) {
	now := s.now()
	t := EnrollmentToken{ID: NewID(), OrganizationID: organizationID, DeviceName: deviceName, SecretPrefix: secretPrefix, CreatedBy: actor, CreatedAt: now, ExpiresAt: now.Add(ttl)}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO enrollment_tokens (enrollment_token_id, organization_id, device_name, secret_hash, secret_prefix, created_by_kind, created_by_id, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`, t.ID, organizationID, deviceName, secretHash, secretPrefix, int16(actor.Kind), actor.ID, now, t.ExpiresAt); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ENROLLMENT_TOKEN_CREATED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ENROLLMENT_TOKEN, TargetID: t.ID, RequestID: requestID,
		})
	})
	return t, err
}

const enrollmentTokenColumns = `enrollment_token_id, organization_id, device_name, secret_prefix, created_by_kind, created_by_id, created_at, expires_at, revoked_at, used_at, coalesce(device_id::text, '')`

func scanEnrollmentToken(r pgx.Row) (EnrollmentToken, error) {
	var t EnrollmentToken
	var kind int16
	err := r.Scan(&t.ID, &t.OrganizationID, &t.DeviceName, &t.SecretPrefix, &kind, &t.CreatedBy.ID, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt, &t.UsedAt, &t.DeviceID)
	t.CreatedBy.Kind = ownsandv1.PrincipalKind(kind)
	return t, err
}

// ListEnrollmentTokens lists the organization's enrollment tokens.
func (s *Store) ListEnrollmentTokens(ctx context.Context, organizationID string, p Page) ([]EnrollmentToken, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+enrollmentTokenColumns+` FROM enrollment_tokens
		WHERE organization_id = $1 AND enrollment_token_id > $2 ORDER BY enrollment_token_id LIMIT $3`, organizationID, afterArg(p), p.Size)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EnrollmentToken, error) { return scanEnrollmentToken(r) })
}

// RevokeEnrollmentToken revokes an unused token. A used or expired token fails
// with ErrConflict; a revoked one succeeds without change.
func (s *Store) RevokeEnrollmentToken(ctx context.Context, organizationID, tokenID string, actor Actor, requestID string) error {
	if !ValidID(tokenID) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		t, err := scanEnrollmentToken(tx.QueryRow(ctx, `SELECT `+enrollmentTokenColumns+` FROM enrollment_tokens WHERE organization_id = $1 AND enrollment_token_id = $2 FOR UPDATE`, organizationID, tokenID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch t.State(s.now()) {
		case ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_REVOKED:
			return nil
		case ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_PENDING:
		default:
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET revoked_at = $2 WHERE enrollment_token_id = $1`, tokenID, s.now()); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ENROLLMENT_TOKEN_REVOKED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ENROLLMENT_TOKEN, TargetID: tokenID, RequestID: requestID,
		})
	})
}

// ErrEnrollmentRejected is the single error of every enrollment failure, so
// that the response does not show which condition failed.
var ErrEnrollmentRejected = errors.New("enrollment token is not valid")

// DefaultDeviceName names a device whose enrollment token has no name.
const DefaultDeviceName = "device"

// Enroll consumes an enrollment token and creates the device in the same
// transaction. A retry with the same token and the same public key before the
// token's expiry returns the same device. Every other failure returns
// ErrEnrollmentRejected.
func (s *Store) Enroll(ctx context.Context, tokenHash, publicKey []byte, caps *ownsandv1.DeviceCapabilities, requestID string) (deviceID, organizationID string, err error) {
	capsBytes, err := proto.MarshalOptions{Deterministic: true}.Marshal(caps)
	if err != nil {
		return "", "", err
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var t EnrollmentToken
		var usedKey []byte
		var kind int16
		err := tx.QueryRow(ctx, `SELECT `+enrollmentTokenColumns+`, used_public_key FROM enrollment_tokens WHERE secret_hash = $1 FOR UPDATE`, tokenHash).
			Scan(&t.ID, &t.OrganizationID, &t.DeviceName, &t.SecretPrefix, &kind, &t.CreatedBy.ID, &t.CreatedAt, &t.ExpiresAt, &t.RevokedAt, &t.UsedAt, &t.DeviceID, &usedKey)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrEnrollmentRejected
		}
		if err != nil {
			return err
		}
		now := s.now()
		if !now.Before(t.ExpiresAt) || t.RevokedAt != nil {
			return ErrEnrollmentRejected
		}
		live, err := orgLiveTx(ctx, tx, t.OrganizationID)
		if err != nil {
			return err
		}
		if !live {
			return ErrEnrollmentRejected
		}
		if t.UsedAt != nil {
			if t.DeviceID != "" && bytes.Equal(usedKey, publicKey) {
				deviceID, organizationID = t.DeviceID, t.OrganizationID
				return nil
			}
			return ErrEnrollmentRejected
		}
		deviceID, organizationID = NewID(), t.OrganizationID
		name := t.DeviceName
		if name == "" {
			name = DefaultDeviceName
		}
		if _, err := tx.Exec(ctx, `INSERT INTO devices (device_id, organization_id, name, state, public_key, capabilities, agent_version, enrolled_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, deviceID, organizationID, name, int16(ownsandv1.DeviceState_DEVICE_STATE_OFFLINE),
			publicKey, capsBytes, caps.GetAgentVersion(), now); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE enrollment_tokens SET used_at = $2, device_id = $3, used_public_key = $4 WHERE enrollment_token_id = $1`,
			t.ID, now, deviceID, publicKey); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_ENROLLED,
			Actor: Actor{ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE, deviceID}, TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_DEVICE,
			TargetID: deviceID, RequestID: requestID,
		})
	})
	return deviceID, organizationID, err
}

func orgLiveTx(ctx context.Context, tx pgx.Tx, organizationID string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM organizations WHERE organization_id = $1 AND deleted_at IS NULL)`, organizationID).Scan(&ok)
	return ok, err
}

// activeStates are the agent states that hold device resources.
var activeStates = []int16{
	int16(ownsandv1.SandboxState_SANDBOX_STATE_CREATING),
	int16(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING),
	int16(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED),
	int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETING),
}

const deviceColumns = `d.device_id, d.organization_id, d.name, d.state, d.public_key, d.capabilities, d.limit_vcpu_count, d.limit_memory_mib, d.limit_disk_gib,
	d.agent_version, d.update_state, d.pending_idle_since, d.state_epoch, d.enrolled_at, d.last_seen_at, d.removed_at,
	coalesce(a.vcpu, 0), coalesce(a.mem, 0), coalesce(a.disk, 0)`

const deviceFrom = `devices d LEFT JOIN LATERAL (
	SELECT sum(vcpu_count) AS vcpu, sum(memory_mib) AS mem, sum(disk_gib) AS disk FROM sandboxes s
	WHERE s.device_id = d.device_id AND s.agent_state = ANY($1)) a ON true`

func scanDevice(r pgx.Row) (Device, error) {
	var d Device
	var state, update int16
	var caps []byte
	err := r.Scan(&d.ID, &d.OrganizationID, &d.Name, &state, &d.PublicKey, &caps, &d.Limits.VCPUCount, &d.Limits.MemoryMiB, &d.Limits.DiskGiB,
		&d.AgentVersion, &update, &d.PendingIdleSince, &d.StateEpoch, &d.EnrolledAt, &d.LastSeenAt, &d.RemovedAt,
		&d.Allocated.VCPUCount, &d.Allocated.MemoryMiB, &d.Allocated.DiskGiB)
	if err != nil {
		return d, err
	}
	d.State = ownsandv1.DeviceState(state)
	d.UpdateState = ownsandv1.AgentUpdateState(update)
	d.Capabilities = &ownsandv1.DeviceCapabilities{}
	if err := proto.Unmarshal(caps, d.Capabilities); err != nil {
		return d, err
	}
	return d, nil
}

// GetDevice returns a device of the organization.
func (s *Store) GetDevice(ctx context.Context, organizationID, deviceID string) (Device, error) {
	if !ValidID(deviceID) {
		return Device{}, ErrNotFound
	}
	d, err := scanDevice(s.Pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM `+deviceFrom+` WHERE d.organization_id = $2 AND d.device_id = $3`,
		activeStates, organizationID, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// GetDeviceByID returns a device without an organization filter. Only the
// agent-facing code uses it, with a device ID that a device credential or a
// device key signature proved.
func (s *Store) GetDeviceByID(ctx context.Context, deviceID string) (Device, error) {
	if !ValidID(deviceID) {
		return Device{}, ErrNotFound
	}
	d, err := scanDevice(s.Pool.QueryRow(ctx, `SELECT `+deviceColumns+` FROM `+deviceFrom+` WHERE d.device_id = $2`, activeStates, deviceID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	return d, err
}

// ListDevices lists the organization's devices; removed devices only on request.
func (s *Store) ListDevices(ctx context.Context, organizationID string, includeRemoved bool, p Page) ([]Device, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+deviceColumns+` FROM `+deviceFrom+`
		WHERE d.organization_id = $2 AND d.device_id > $3 AND ($5 OR d.state <> $6)
		ORDER BY d.device_id LIMIT $4`, activeStates, organizationID, afterArg(p), p.Size, includeRemoved, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Device, error) { return scanDevice(r) })
}

// UpdateDeviceLimits sets the device limits and writes the configuration
// event. A removed device fails with ErrConflict.
func (s *Store) UpdateDeviceLimits(ctx context.Context, organizationID, deviceID string, limits Limits, actor Actor, requestID string) error {
	if !ValidID(deviceID) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var state int16
		err := tx.QueryRow(ctx, `SELECT state FROM devices WHERE organization_id = $1 AND device_id = $2 FOR UPDATE`, organizationID, deviceID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE devices SET limit_vcpu_count = $2, limit_memory_mib = $3, limit_disk_gib = $4 WHERE device_id = $1`,
			deviceID, limits.VCPUCount, limits.MemoryMiB, limits.DiskGiB); err != nil {
			return err
		}
		if err := s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventDeviceLimitsChanged, OrganizationID: organizationID, DeviceID: deviceID}); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_LIMITS_CHANGED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_DEVICE, TargetID: deviceID, RequestID: requestID,
		})
	})
}

// RemoveDevice marks the device removed, invalidates its session credentials,
// creates its removal tombstone, ends its non-terminal sandboxes with
// DEVICE_REMOVED, drops the records of snapshots that exist only on it, and
// writes the removal control event, in one transaction. Removing a removed
// device succeeds without change.
func (s *Store) RemoveDevice(ctx context.Context, organizationID, deviceID string, actor Actor, requestID string) error {
	if !ValidID(deviceID) {
		return ErrNotFound
	}
	return s.tx(ctx, func(tx pgx.Tx) error {
		var state int16
		err := tx.QueryRow(ctx, `SELECT state FROM devices WHERE organization_id = $1 AND device_id = $2 FOR UPDATE`, organizationID, deviceID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			return nil
		}
		if err := s.removeDeviceTx(ctx, tx, organizationID, deviceID, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED, actor, requestID); err != nil {
			return err
		}
		return s.audit(ctx, tx, AuditEvent{
			OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_REMOVED, Actor: actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_DEVICE, TargetID: deviceID, RequestID: requestID,
		})
	})
}

func (s *Store) removeDeviceTx(ctx context.Context, tx pgx.Tx, organizationID, deviceID string, reason ownsandv1.SandboxEndReason, actor Actor, requestID string) error {
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE devices SET state = $2, removed_at = $3 WHERE device_id = $1`, deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED), now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM device_sessions WHERE device_id = $1`, deviceID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO device_tombstones (device_id, public_key, expires_at)
		SELECT device_id, public_key, $2 FROM devices WHERE device_id = $1
		ON CONFLICT (device_id) DO NOTHING`, deviceID, now.Add(TombstoneLifetime)); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT sandbox_id FROM sandboxes WHERE organization_id = $1 AND device_id = $2 AND agent_state NOT IN ($3, $4) FOR UPDATE`,
		organizationID, deviceID, int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED), int16(ownsandv1.SandboxState_SANDBOX_STATE_FAILED))
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := s.endSandboxTx(ctx, tx, organizationID, id, ownsandv1.SandboxState_SANDBOX_STATE_DELETED, reason, actor, requestID); err != nil {
			return err
		}
	}
	if err := s.dropDeviceSnapshotsTx(ctx, tx, organizationID, deviceID); err != nil {
		return err
	}
	return s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventDeviceRemoved, OrganizationID: organizationID, DeviceID: deviceID})
}

// endSandboxTx moves a non-terminal sandbox to a terminal state and writes its
// single SANDBOX_DELETED audit event. The actor of a pending delete request
// takes precedence over actor. It reports whether the sandbox changed.
func (s *Store) endSandboxTx(ctx context.Context, tx pgx.Tx, organizationID, sandboxID string, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason, actor Actor, requestID string) (bool, error) {
	var deleteActorKind int16
	var deleteActorID string
	var pending bool
	err := tx.QueryRow(ctx, `UPDATE sandboxes SET agent_state = $3, end_reason = $4, ended_at = $5
		WHERE organization_id = $1 AND sandbox_id = $2 AND agent_state NOT IN ($6, $7)
		RETURNING pending_delete, delete_actor_kind, delete_actor_id`,
		organizationID, sandboxID, int16(state), int16(reason), s.now(),
		int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED), int16(ownsandv1.SandboxState_SANDBOX_STATE_FAILED)).Scan(&pending, &deleteActorKind, &deleteActorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if pending && ownsandv1.PrincipalKind(deleteActorKind) != ownsandv1.PrincipalKind_PRINCIPAL_KIND_UNSPECIFIED {
		actor = Actor{ownsandv1.PrincipalKind(deleteActorKind), deleteActorID}
	}
	return true, s.audit(ctx, tx, AuditEvent{
		OrganizationID: organizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_DELETED, Actor: actor, EndReason: reason,
		TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_SANDBOX, TargetID: sandboxID, RequestID: requestID,
	})
}

// SnapshotPrefix is the R2 object key prefix of a snapshot.
func SnapshotPrefix(organizationID, snapshotID string) string {
	return "organizations/" + organizationID + "/snapshots/" + snapshotID + "/"
}

// OrganizationPrefix is the R2 object key prefix of an organization.
func OrganizationPrefix(organizationID string) string {
	return "organizations/" + organizationID + "/"
}

// dropDeviceSnapshotsTx removes the local copies of the device's snapshots: an
// exported snapshot keeps its record without a device; any other snapshot
// record is deleted with its templates, and a started export is cancelled
// through an R2 clean-up job.
func (s *Store) dropDeviceSnapshotsTx(ctx context.Context, tx pgx.Tx, organizationID, deviceID string) error {
	if _, err := tx.Exec(ctx, `UPDATE snapshots SET device_id = NULL WHERE organization_id = $1 AND device_id = $2 AND export_state = $3`,
		organizationID, deviceID, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `DELETE FROM snapshots WHERE organization_id = $1 AND device_id = $2 RETURNING snapshot_id, export_state`, organizationID, deviceID)
	if err != nil {
		return err
	}
	type dropped struct {
		id    string
		state int16
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (dropped, error) {
		var d dropped
		return d, r.Scan(&d.id, &d.state)
	})
	if err != nil {
		return err
	}
	for _, d := range list {
		if ownsandv1.SnapshotExportState(d.state) != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED {
			if err := s.cleanupJobTx(ctx, tx, organizationID, SnapshotPrefix(organizationID, d.id), d.id); err != nil {
				return err
			}
		}
	}
	return nil
}

// cleanupJobTx records an R2 clean-up job for a prefix. The final sweep runs
// after the last signed upload URL for the snapshot (or, for an organization
// prefix, any snapshot of the organization) has expired.
func (s *Store) cleanupJobTx(ctx context.Context, tx pgx.Tx, organizationID, prefix, snapshotID string) error {
	var last *time.Time
	var err error
	if snapshotID != "" {
		err = tx.QueryRow(ctx, `SELECT max(last_expires_at) FROM signed_upload_issues WHERE organization_id = $1 AND snapshot_id = $2`, organizationID, snapshotID).Scan(&last)
	} else {
		err = tx.QueryRow(ctx, `SELECT max(last_expires_at) FROM signed_upload_issues WHERE organization_id = $1`, organizationID).Scan(&last)
	}
	if err != nil {
		return err
	}
	after := s.now()
	if last != nil && last.After(after) {
		after = *last
	}
	_, err = tx.Exec(ctx, `INSERT INTO r2_cleanup_jobs (cleanup_job_id, organization_id, prefix, final_sweep_after, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (prefix) DO UPDATE SET final_sweep_after = greatest(r2_cleanup_jobs.final_sweep_after, EXCLUDED.final_sweep_after)`,
		NewID(), organizationID, prefix, after, s.now())
	return err
}
