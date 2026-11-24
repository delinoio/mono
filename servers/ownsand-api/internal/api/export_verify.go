// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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
	t := time.NewTicker(c.S.exportVerifyPoll())
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if c.held() {
			c.RunExportVerifyOnce(ctx)
		}
	}
}

// RunExportVerifyOnce makes one verifier pass over the EXPORTED snapshots that
// are not verified yet. A row without export_verify_after (exported before
// the migration or by a release that does not set it) gets it set to now plus
// the upload URL lifetime and is not listed in this pass. An R2 failure, or a
// control plane without R2, changes nothing: a failed export would otherwise
// follow from an outage.
func (c *Control) RunExportVerifyOnce(ctx context.Context) {
	s := c.S
	if s.Objects == nil {
		c.noExportVerifyStore.Do(func() {
			s.Logger.Warn("export_verification_retry", slog.String("reason", "r2_not_configured"))
		})
		return
	}
	jobs, err := s.claimExportVerifyJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			s.Logger.Warn("export_verification_scan_failed", slog.Any("error", err))
		}
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		prefix := attemptPrefix(job.orgID, job.snapshotID, job.commandID)
		listCtx, cancel := context.WithTimeout(ctx, exportVerifyListTimeout)
		objects, err := s.Objects.ListPrefix(listCtx, prefix)
		cancel()
		if err != nil {
			s.Logger.Warn("export_verification_retry", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID),
				slog.String("reason", "r2_list_failed"), slog.Any("error", err))
			continue
		}
		if err := s.applyExportVerification(ctx, job, objects); err != nil {
			if ctx.Err() == nil {
				s.Logger.Warn("export_verification_retry", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID),
					slog.String("reason", "database_error"), slog.Any("error", err))
			}
		}
	}
}

// claimExportVerifyJobs sets the verification time of the rows that have none
// and returns the rows whose time passed. It holds the rows with
// FOR UPDATE SKIP LOCKED only while it reads them; the R2 listing runs
// without a lock, and applyExportVerification locks the row again and checks
// that it still names the same attempt.
func (s *Service) claimExportVerifyJobs(ctx context.Context) ([]exportVerifyJob, error) {
	var jobs []exportVerifyJob
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		jobs = nil
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
		if err := rows.Err(); err != nil {
			return err
		}
		for _, r := range due {
			if r.after == nil {
				// First sight of a row that no verification time was set for:
				// the signed URLs of its attempt may be valid for another
				// lifetime, so wait for it before the first listing.
				after := now.Add(s.exportVerifyDelay())
				if _, err := tx.Exec(ctx, "UPDATE snapshots SET export_verify_after = $2 WHERE snapshot_id = $1", r.snapshotID, after); err != nil {
					return err
				}
				s.Logger.InfoContext(ctx, "export_verification_scheduled", slog.String("snapshot_id", r.snapshotID), slog.Time("verify_after", after))
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
	return jobs, err
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
func (s *Service) applyExportVerification(ctx context.Context, job exportVerifyJob, objects []objectstore.Object) error {
	reason, key := compareExport(job, objects)
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
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
		if reason == "" {
			if _, err := tx.Exec(ctx, "UPDATE snapshots SET export_verified = true WHERE snapshot_id = $1", job.snapshotID); err != nil {
				return err
			}
			s.Logger.InfoContext(ctx, "export_verified", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID), slog.Int("parts", len(job.parts)))
			return nil
		}
		if _, err := tx.Exec(ctx, "UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1", job.snapshotID,
			int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, "DELETE FROM snapshot_parts WHERE snapshot_id = $1", job.snapshotID); err != nil {
			return err
		}
		if err := s.startCleanupJobTx(ctx, tx, job.orgID, attemptPrefix(job.orgID, job.snapshotID, job.commandID)); err != nil {
			return err
		}
		s.Logger.WarnContext(ctx, "export_verification_failed", slog.String("snapshot_id", job.snapshotID), slog.String("command_id", job.commandID),
			slog.String("reason", string(reason)), slog.String("object_key", key))
		return nil
	})
}
