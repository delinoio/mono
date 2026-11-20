// SPDX-License-Identifier: FSL-1.1-ALv2

// Package janitor runs the retention jobs of the control plane: expired
// nonces, device sessions and idempotency keys, device tombstones whose
// expiry passed, audit events older than 90 days, ended sandboxes older
// than 30 days that no longer reserve a macOS machine identifier, and
// processed control events.
package janitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// DefaultInterval is the interval of the retention jobs. The retention
// periods are in the job queries: audit events 90 days, ended sandboxes 30
// days, processed control events 1 hour.
const DefaultInterval = time.Hour

type job struct {
	name string
	sql  string
}

func jobs() []job {
	return []job{
		{"consumed_nonces", "DELETE FROM consumed_nonces WHERE expire_time <= $1"},
		{"device_sessions", "DELETE FROM device_sessions WHERE expire_time <= $1"},
		{"sandbox_create_keys", "DELETE FROM sandbox_create_keys WHERE expire_time <= $1"},
		// Lookups already treat an expired tombstone as absent; this deletes
		// it, also when its scheduled deletion was missed.
		{"device_tombstones", "DELETE FROM devices WHERE state = 3 /* REMOVED */ AND tombstone_expire_time <= $1"},
		{"audit_events", "DELETE FROM audit_events WHERE create_time < $1::timestamptz - interval '90 days'"},
		// A sandbox record stays while it reserves a macOS machine identifier
		// (its max_ttl deadline has not passed).
		{"ended_sandboxes", `DELETE FROM sandboxes WHERE end_time IS NOT NULL AND end_time < $1::timestamptz - interval '30 days'
			AND (machine_lineage IS NULL OR max_deadline <= $1::timestamptz)`},
		{"control_events", "DELETE FROM control_events WHERE create_time < $1::timestamptz - interval '1 hour'"},
	}
}

// RunOnce runs every job once at now.
func RunOnce(ctx context.Context, d *db.DB, logger *slog.Logger, now time.Time) {
	for _, j := range jobs() {
		tag, err := d.Pool.Exec(ctx, j.sql, now)
		if err != nil {
			logger.WarnContext(ctx, "janitor_job_failed", slog.String("job", j.name), slog.Any("error", err))
			continue
		}
		if n := tag.RowsAffected(); n > 0 {
			logger.InfoContext(ctx, "janitor_job_deleted", slog.String("job", j.name), slog.Int64("rows", n))
		}
	}
}

// Run runs the jobs every interval until ctx ends.
func Run(ctx context.Context, d *db.DB, logger *slog.Logger, now func() time.Time, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		RunOnce(ctx, d, logger, now())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
