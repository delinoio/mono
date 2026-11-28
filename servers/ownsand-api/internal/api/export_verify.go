// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/lease"
	"github.com/delinoio/mono/servers/ownsand-api/internal/objectstore"
)

// Export verification. The control plane bills snapshot storage from the part
// sizes that the agent reports, so after every upload URL of an export attempt
// expired it lists the attempt's prefix in R2 and compares the objects with
// the manifest. A snapshot is EXPORTED, importable and billed from the
// reported sizes immediately; a failed verification turns it into
// EXPORT_FAILED and removes its manifest, so billing stops counting the parts.
// The owning contract is the Metering and Export recovery sections of
// docs/servers-ownsand-api-foundation.md.

// exportVerifyBatch bounds the rows that one verifier pass handles. Rows that
// an earlier release exported are all unverified after the migration, so the
// bound spreads their R2 listings (class A operations) over several passes.
const exportVerifyBatch = 50

// exportVerifyListTimeout bounds one R2 listing of the verifier.
const exportVerifyListTimeout = 60 * time.Second

var errExportVerifyLeaseEnded = errors.New("export verification lease period ended")

// ExportVerifyReason names why a verification failed.
type ExportVerifyReason string

// The reasons of the export_verification_failed log.
const (
	// ExportVerifyMissingPart means that a manifest part has no object.
	ExportVerifyMissingPart ExportVerifyReason = "missing_part"
	// ExportVerifySizeMismatch means that an object has another size than the
	// manifest part.
	ExportVerifySizeMismatch ExportVerifyReason = "size_mismatch"
	// ExportVerifyUnlistedObject means that the attempt prefix holds an
	// object that the manifest does not list.
	ExportVerifyUnlistedObject ExportVerifyReason = "unlisted_object"
)

// DefaultExportVerifyPoll is the interval of the export verifier.
const DefaultExportVerifyPoll = 30 * time.Second

func (s *Service) exportVerifyPoll() time.Duration {
	if s.Timing.ExportVerifyPoll > 0 {
		return s.Timing.ExportVerifyPoll
	}
	return DefaultExportVerifyPoll
}

// exportVerifyDelay is the wait between the completion of an export and the
// first listing: the lifetime of the upload URLs, so no URL of the attempt
// can still write an object.
func (s *Service) exportVerifyDelay() time.Duration {
	if s.Timing.ExportVerifyDelay > 0 {
		return s.Timing.ExportVerifyDelay
	}
	return signedURLLifetime
}

// exportVerifyJob is an unverified EXPORTED snapshot that is due.
type exportVerifyJob struct {
	orgID, snapshotID, commandID string
	// parts maps the part numbers of the manifest to their reported sizes.
	parts map[int32]int64
}

// runExportVerifier verifies the exports while the instance holds the lease.
func (c *Control) runExportVerifier(ctx context.Context) {
	ticks := c.ExportVerifyTicks
	if ticks == nil {
		ticker := time.NewTicker(c.S.exportVerifyPoll())
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
		}
		c.RunExportVerifyOnce(ctx)
	}
}

// RunExportVerifyOnce makes one verifier pass during the current lease period
// over EXPORTED snapshots that are not verified yet. The pass schedules rows
// without export_verify_after for the end of the upload URL lifetime and skips
// listing them. A listing or apply error leaves the export unverified so a
// later pass can retry it.
func (c *Control) RunExportVerifyOnce(parent context.Context) {
	s := c.S
	if s.Lease == nil {
		return
	}
	var period *lease.Period
	s.Lease.WhileHeld(func(held *lease.Period) { period = held })
	if period == nil {
		return
	}
	ctx, cancel := period.ContextUntilLoss(parent)
	defer cancel()
	if s.Objects == nil {
		c.noExportVerifyStore.Do(func() {
			s.Logger.Warn("export_verification_retry", slog.String("reason", "r2_not_configured"))
		})
		return
	}
	jobs, err := s.claimExportVerifyJobs(ctx, period)
	if err != nil {
		if ctx.Err() == nil && !errors.Is(err, errExportVerifyLeaseEnded) {
			s.Logger.Warn("export_verification_scan_failed", slog.Any("error", err))
		}
		return
	}
	// Check the captured period directly. Its context may still be waiting
	// for cancellation to propagate, or this instance may have reacquired.
	stillHeld := func() bool {
		var current bool
		s.Lease.WhileHeld(func(held *lease.Period) { current = held == period })
		return current
	}
	for _, job := range jobs {
		if ctx.Err() != nil || !stillHeld() {
			return
		}
		prefix := attemptPrefix(job.orgID, job.snapshotID, job.commandID)
		listCtx, cancelList := context.WithTimeout(ctx, exportVerifyListTimeout)
		objects, err := s.Objects.ListPrefix(listCtx, prefix)
		cancelList()
		if ctx.Err() != nil || !stillHeld() {
			return
		}
		if err != nil {
			s.Logger.Warn("export_verification_retry", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID),
				slog.String("reason", "r2_list_failed"), slog.Any("error", err))
			continue
		}
		err = s.applyExportVerification(ctx, period, job, objects)
		if errors.Is(err, errExportVerifyLeaseEnded) {
			return
		}
		if err != nil && ctx.Err() == nil {
			s.Logger.Warn("export_verification_retry", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID),
				slog.String("reason", "database_error"), slog.Any("error", err))
		}
	}
}

// claimExportVerifyJobs schedules the first check for rows without a deadline
// and returns jobs whose deadline has passed. It advances the next claim time
// before releasing the row locks so errors cannot keep other due jobs waiting.
// The R2 listing runs outside the transaction. When applying a result, the
// verifier locks the row again and checks that it still names the same attempt.
func (s *Service) claimExportVerifyJobs(ctx context.Context, period *lease.Period) ([]exportVerifyJob, error) {
	type scheduledVerification struct {
		snapshotID string
		after      time.Time
	}
	var jobs []exportVerifyJob
	var scheduled []scheduledVerification
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		now := s.now()
		type row struct {
			snapshotID, orgID, commandID string
			after                        *time.Time
		}
		rows, err := tx.Query(ctx, `SELECT snapshot_id::text, organization_id::text, COALESCE(export_command_id::text, ''), export_verify_after
			FROM snapshots WHERE export_state = $1 AND NOT export_verified AND (export_verify_after IS NULL OR export_verify_after <= $2)
			ORDER BY export_verify_after NULLS FIRST, snapshot_id LIMIT $3 FOR UPDATE SKIP LOCKED`,
			int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED), now, exportVerifyBatch)
		if err != nil {
			return err
		}
		var due []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.snapshotID, &r.orgID, &r.commandID, &r.after); err != nil {
				rows.Close()
				return err
			}
			due = append(due, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(due) == 0 {
			return err
		}
		// Lock snapshots first so a wait on them cannot stall takeover.
		// The shared lease lock serializes the claim commit with takeover.
		expiry, err := period.LockRow(ctx, tx)
		if err != nil {
			return err
		}
		if expiry.IsZero() {
			return errExportVerifyLeaseEnded
		}
		retryAfter := now.Add(s.exportVerifyPoll())
		for _, r := range due {
			after := retryAfter
			if r.after == nil {
				// Upload URLs can still be valid for a full lifetime.
				after = now.Add(s.exportVerifyDelay())
			}
			err := tx.QueryRow(ctx, `UPDATE snapshots SET export_verify_after = $2 WHERE snapshot_id = $1
				AND clock_timestamp() < $3::timestamptz RETURNING export_verify_after`, r.snapshotID, after, expiry).Scan(&after)
			if errors.Is(err, pgx.ErrNoRows) {
				return errExportVerifyLeaseEnded
			}
			if err != nil {
				return err
			}
			if r.after == nil {
				scheduled = append(scheduled, scheduledVerification{snapshotID: r.snapshotID, after: after})
				continue
			}
			parts, err := loadManifest(ctx, tx, r.snapshotID)
			if err != nil {
				return err
			}
			jobs = append(jobs, exportVerifyJob{orgID: r.orgID, snapshotID: r.snapshotID, commandID: r.commandID, parts: parts})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, r := range scheduled {
		s.Logger.InfoContext(ctx, "export_verification_scheduled", slog.String("snapshot_id", r.snapshotID), slog.Time("verify_after", r.after.UTC()))
	}
	return jobs, nil
}

// loadManifest returns the part sizes that the agent reported for snapshotID.
func loadManifest(ctx context.Context, q pgx.Tx, snapshotID string) (map[int32]int64, error) {
	rows, err := q.Query(ctx, "SELECT part_number, size_bytes FROM snapshot_parts WHERE snapshot_id = $1", snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parts := map[int32]int64{}
	for rows.Next() {
		var n int32
		var size int64
		if err := rows.Scan(&n, &size); err != nil {
			return nil, err
		}
		parts[n] = size
	}
	return parts, rows.Err()
}

// compareExport compares the listed objects of an attempt with its manifest.
// It returns the reason of the first difference and the object key it concerns,
// checking the missing parts, then the sizes, then the unlisted objects, or an
// empty reason when the keys equal the manifest's part keys and every size
// matches.
func compareExport(job exportVerifyJob, objects []objectstore.Object) (ExportVerifyReason, string) {
	sizes := make(map[string]int64, len(objects))
	for _, o := range objects {
		sizes[o.Key] = o.Size
	}
	numbers := make([]int32, 0, len(job.parts))
	for n := range job.parts {
		numbers = append(numbers, n)
	}
	slices.Sort(numbers)
	manifest := make(map[string]struct{}, len(numbers))
	for _, n := range numbers {
		key := partKey(job.orgID, job.snapshotID, job.commandID, n)
		if _, listed := sizes[key]; !listed {
			return ExportVerifyMissingPart, key
		}
		manifest[key] = struct{}{}
	}
	for _, n := range numbers {
		key := partKey(job.orgID, job.snapshotID, job.commandID, n)
		if sizes[key] != job.parts[n] {
			return ExportVerifySizeMismatch, key
		}
	}
	for _, o := range objects {
		if _, ok := manifest[o.Key]; !ok {
			return ExportVerifyUnlistedObject, o.Key
		}
	}
	return "", ""
}

// applyExportVerification records the outcome of a verification. It locks the
// row again and does nothing when the snapshot was deleted, is no longer
// EXPORTED, was exported again, or was verified meanwhile. A match marks the
// row verified. A difference fails the export like a failed export attempt:
// the snapshot becomes EXPORT_FAILED, its manifest is removed so that billing
// stops counting the reported sizes, and a clean-up job deletes the attempt
// prefix.
func (s *Service) applyExportVerification(ctx context.Context, period *lease.Period, job exportVerifyJob, objects []objectstore.Object) error {
	reason, key := compareExport(job, objects)
	var applied bool
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		var export int16
		var owner string
		var verified bool
		err := tx.QueryRow(ctx, "SELECT export_state, COALESCE(export_command_id::text, ''), export_verified FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", job.snapshotID).
			Scan(&export, &owner, &verified)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted meanwhile; its clean-up job removes the parts
		}
		if err != nil {
			return err
		}
		if ownsandv1.SnapshotExportState(export) != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED || owner != job.commandID || verified {
			return nil
		}
		// The snapshot row is already locked. Check the lease at the
		// write and hold its shared lock through the result's commit.
		expiry, err := period.LockRow(ctx, tx)
		if err != nil {
			return err
		}
		if expiry.IsZero() {
			return errExportVerifyLeaseEnded
		}
		var updated pgconn.CommandTag
		if reason == "" {
			updated, err = tx.Exec(ctx, `UPDATE snapshots SET export_verified = true WHERE snapshot_id = $1
				AND clock_timestamp() < $2::timestamptz`, job.snapshotID, expiry)
		} else {
			updated, err = tx.Exec(ctx, `UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1
				AND clock_timestamp() < $3::timestamptz`, job.snapshotID,
				int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED), expiry)
		}
		if err != nil {
			return err
		}
		if updated.RowsAffected() == 0 {
			return errExportVerifyLeaseEnded
		}
		if reason != "" {
			if _, err := tx.Exec(ctx, "DELETE FROM snapshot_parts WHERE snapshot_id = $1", job.snapshotID); err != nil {
				return err
			}
			if err := s.startCleanupJobTx(ctx, tx, job.orgID, attemptPrefix(job.orgID, job.snapshotID, job.commandID)); err != nil {
				return err
			}
		}
		applied = true
		return nil
	})
	if err != nil || !applied {
		return err
	}
	if reason == "" {
		s.Logger.InfoContext(ctx, "export_verified", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID), slog.Int("parts", len(job.parts)))
		return nil
	}
	s.Logger.WarnContext(ctx, "export_verification_failed", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID),
		slog.String("reason", string(reason)), slog.String("object_key", key))
	return nil
}
