// SPDX-License-Identifier: FSL-1.1-ALv2

// Package jobs runs the periodic maintenance of the control plane: the daily
// retention job (audit events after 90 days, ended sandboxes after 30 days,
// expired tombstones, sessions, nonces, idempotency keys and control events),
// the tombstone deletion at each tombstone's expires_at, and the R2 clean-up
// jobs.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// ObjectStore deletes every object (and aborts every multipart upload)
// under a prefix.
type ObjectStore interface {
	DeletePrefix(ctx context.Context, prefix string) error
}

// Intervals.
const (
	RetentionInterval    = 24 * time.Hour
	MaxTombstoneSleep    = time.Hour
	CleanupInterval      = time.Minute
	CleanupWarnThreshold = 10
)

// Runner runs the jobs.
type Runner struct {
	Store  *store.Store
	Logger *slog.Logger
	// Objects is nil when R2 is not configured; clean-up jobs then stay
	// pending and are logged.
	Objects ObjectStore
}

// Run starts every loop and blocks until ctx ends.
func (r *Runner) Run(ctx context.Context) {
	go r.loop(ctx, RetentionInterval, r.retention)
	go r.tombstones(ctx)
	r.loop(ctx, CleanupInterval, r.cleanup)
}

func (r *Runner) loop(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	fn(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

func (r *Runner) retention(ctx context.Context) {
	res, err := r.Store.RunRetention(ctx)
	if err != nil {
		r.Logger.Error("retention_failed", slog.Any("error", err))
		return
	}
	r.Logger.Info("retention_completed",
		slog.Int64("audit_events", res.AuditEvents), slog.Int64("ended_sandboxes", res.EndedSandboxes),
		slog.Int64("tombstones", res.Tombstones), slog.Int64("device_sessions", res.DeviceSessions),
		slog.Int64("consumed_nonces", res.ConsumedNonces), slog.Int64("control_events", res.ControlEvents),
		slog.Int64("idempotency_keys", res.IdempotencyKeys))
}

// tombstones deletes each tombstone at its expires_at. Lookups already treat
// an expired tombstone as absent; the daily retention run deletes any whose
// scheduled deletion was missed.
func (r *Runner) tombstones(ctx context.Context) {
	for {
		sleep := MaxTombstoneSleep
		next, ok, err := r.Store.NextTombstoneExpiry(ctx)
		if err != nil {
			r.Logger.Warn("tombstone_schedule_failed", slog.Any("error", err))
		} else if ok {
			if d := time.Until(next); d < sleep {
				sleep = max(d, time.Second)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(sleep):
		}
		if ok && !time.Now().Before(next) {
			if _, err := r.Store.RunRetention(ctx); err != nil {
				r.Logger.Warn("tombstone_delete_failed", slog.Any("error", err))
			}
		}
	}
}

func (r *Runner) cleanup(ctx context.Context) {
	jobs, err := r.Store.DueCleanupJobs(ctx)
	if err != nil {
		r.Logger.Warn("r2_cleanup_list_failed", slog.Any("error", err))
		return
	}
	for _, j := range jobs {
		if r.Objects == nil {
			r.Logger.Warn("r2_cleanup_pending_unconfigured", slog.String("cleanup_job_id", j.ID), slog.String("organization_id", j.OrganizationID))
			continue
		}
		started := time.Now()
		err := r.Objects.DeletePrefix(ctx, j.Prefix)
		if err != nil {
			r.Logger.Warn("r2_cleanup_failed", slog.String("cleanup_job_id", j.ID), slog.String("organization_id", j.OrganizationID),
				slog.Int("attempts", j.Attempts+1), slog.Any("error", err))
		}
		done, recErr := r.Store.RecordCleanupAttempt(ctx, j.ID, started, err)
		if recErr != nil {
			r.Logger.Warn("r2_cleanup_record_failed", slog.String("cleanup_job_id", j.ID), slog.Any("error", recErr))
			continue
		}
		if done {
			r.Logger.Info("r2_cleanup_completed", slog.String("cleanup_job_id", j.ID), slog.String("organization_id", j.OrganizationID))
		}
	}
}
