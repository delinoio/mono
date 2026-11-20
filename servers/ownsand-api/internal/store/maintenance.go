// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Retention periods. Contract: Storage, Audit events and Device removal.
const (
	AuditRetention        = 90 * 24 * time.Hour
	EndedSandboxRetention = 30 * 24 * time.Hour
	IdempotencyRetention  = 24 * time.Hour
	ControlEventRetention = 24 * time.Hour
)

// RetentionResult counts the rows that a retention run deleted.
type RetentionResult struct {
	AuditEvents      int64
	EndedSandboxes   int64
	Tombstones       int64
	DeviceSessions   int64
	ConsumedNonces   int64
	ControlEvents    int64
	IdempotencyKeys  int64
	SignedUploadRows int64
}

// RunRetention deletes expired rows. The daily job and the tombstone
// scheduler call it; it is idempotent.
//
// An ended sandbox that still reserves a macOS machine identifier (its
// device's removal was not confirmed and its max_ttl deadline has not passed)
// is kept beyond 30 days.
func (s *Store) RunRetention(ctx context.Context) (RetentionResult, error) {
	now := s.now()
	var r RetentionResult
	exec := func(dst *int64, sql string, args ...any) error {
		tag, err := s.Pool.Exec(ctx, sql, args...)
		if err != nil {
			return err
		}
		*dst = tag.RowsAffected()
		return nil
	}
	steps := []func() error{
		func() error {
			return exec(&r.AuditEvents, `DELETE FROM audit_events WHERE created_at < $1`, now.Add(-AuditRetention))
		},
		func() error {
			return exec(&r.EndedSandboxes, `DELETE FROM sandboxes s WHERE s.agent_state IN ($1, $2) AND s.ended_at < $3
				AND NOT (s.macos_machine_identifier IS NOT NULL AND s.max_deadline > $4 AND s.end_reason = $5
					AND EXISTS (SELECT 1 FROM devices d WHERE d.device_id = s.device_id AND d.removal_confirmed_at IS NULL))`,
				int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED), int16(ownsandv1.SandboxState_SANDBOX_STATE_FAILED),
				now.Add(-EndedSandboxRetention), now, int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED))
		},
		func() error {
			return exec(&r.Tombstones, `DELETE FROM device_tombstones WHERE expires_at <= $1`, now)
		},
		func() error {
			return exec(&r.DeviceSessions, `DELETE FROM device_sessions WHERE expires_at <= $1`, now)
		},
		func() error {
			return exec(&r.ConsumedNonces, `DELETE FROM consumed_nonces WHERE expires_at <= $1`, now)
		},
		func() error {
			return exec(&r.ControlEvents, `DELETE FROM control_events WHERE created_at < $1`, now.Add(-ControlEventRetention))
		},
		func() error {
			return exec(&r.IdempotencyKeys, `UPDATE sandboxes SET idempotency_key = NULL, idempotency_hash = NULL, idempotency_expires_at = NULL
				WHERE idempotency_expires_at <= $1`, now)
		},
		func() error {
			return exec(&r.SignedUploadRows, `DELETE FROM signed_upload_issues WHERE last_expires_at < $1
				AND NOT EXISTS (SELECT 1 FROM organizations o WHERE o.organization_id = signed_upload_issues.organization_id AND o.deleted_at IS NOT NULL)`, now.Add(-time.Hour))
		},
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return r, err
		}
	}
	return r, nil
}

// NextTombstoneExpiry returns the earliest tombstone expiry, if any, so that
// the scheduler can delete each tombstone at its expires_at.
func (s *Store) NextTombstoneExpiry(ctx context.Context) (time.Time, bool, error) {
	var t *time.Time
	if err := s.Pool.QueryRow(ctx, `SELECT min(expires_at) FROM device_tombstones`).Scan(&t); err != nil {
		return time.Time{}, false, err
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// LiveTombstone returns the public key of a device's removal tombstone that
// has not expired.
func (s *Store) LiveTombstone(ctx context.Context, deviceID string) ([]byte, bool, error) {
	if !ValidID(deviceID) {
		return nil, false, nil
	}
	var key []byte
	err := s.Pool.QueryRow(ctx, `SELECT public_key FROM device_tombstones WHERE device_id = $1 AND expires_at > $2`, deviceID, s.now()).Scan(&key)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	return key, err == nil, err
}

// ConfirmRemoval deletes the device's removal tombstone and records the
// confirmation on the removed device row (which ends its macOS identifier
// reservations).
func (s *Store) ConfirmRemoval(ctx context.Context, deviceID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM device_tombstones WHERE device_id = $1 AND expires_at > $2`, deviceID, s.now())
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return ErrNotFound
		}
		_, err = tx.Exec(ctx, `UPDATE devices SET removal_confirmed_at = $2 WHERE device_id = $1`, deviceID, s.now())
		return err
	})
}

// ConsumeNonce records a nonce until its expiry. It fails with
// ErrAlreadyExists when the nonce was consumed before.
func (s *Store) ConsumeNonce(ctx context.Context, nonceHash []byte, expiresAt time.Time) error {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO consumed_nonces (nonce_hash, expires_at) VALUES ($1, $2) ON CONFLICT DO NOTHING`, nonceHash, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrAlreadyExists
	}
	return nil
}

// CreateDeviceSession stores the hash of a new device session credential and
// the agent's state epoch. A removed device fails with ErrConflict.
func (s *Store) CreateDeviceSession(ctx context.Context, deviceID string, credentialHash []byte, expiresAt time.Time, stateEpoch string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var orgID string
		var state int16
		err := tx.QueryRow(ctx, `SELECT organization_id, state FROM devices WHERE device_id = $1 FOR UPDATE`, deviceID).Scan(&orgID, &state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `UPDATE devices SET state_epoch = $2 WHERE device_id = $1`, deviceID, stateEpoch); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO device_sessions (credential_hash, device_id, organization_id, created_at, expires_at) VALUES ($1, $2, $3, $4, $5)`,
			credentialHash, deviceID, orgID, s.now(), expiresAt)
		return err
	})
}

// CleanupJob is an R2 clean-up job.
type CleanupJob struct {
	ID              string
	OrganizationID  string
	Prefix          string
	FinalSweepAfter time.Time
	Attempts        int
}

// DueCleanupJobs returns every clean-up job.
func (s *Store) DueCleanupJobs(ctx context.Context) ([]CleanupJob, error) {
	rows, err := s.Pool.Query(ctx, `SELECT cleanup_job_id, organization_id, prefix, final_sweep_after, attempts FROM r2_cleanup_jobs ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (CleanupJob, error) {
		var j CleanupJob
		return j, r.Scan(&j.ID, &j.OrganizationID, &j.Prefix, &j.FinalSweepAfter, &j.Attempts)
	})
}

// RecordCleanupAttempt records the result of one sweep. A successful sweep
// that started after the job's final sweep time deletes the job state.
func (s *Store) RecordCleanupAttempt(ctx context.Context, id string, sweepStarted time.Time, sweepErr error) (done bool, err error) {
	if sweepErr != nil {
		_, err = s.Pool.Exec(ctx, `UPDATE r2_cleanup_jobs SET attempts = attempts + 1, last_error = $2 WHERE cleanup_job_id = $1`, id, "sweep failed")
		return false, err
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM r2_cleanup_jobs WHERE cleanup_job_id = $1 AND final_sweep_after <= $2`, id, sweepStarted)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	_, err = s.Pool.Exec(ctx, `UPDATE r2_cleanup_jobs SET attempts = attempts + 1, last_error = '' WHERE cleanup_job_id = $1`, id)
	return false, err
}
