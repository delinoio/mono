// SPDX-License-Identifier: FSL-1.1-ALv2

// Package janitor runs the retention jobs of the control plane: expired
// nonces, device sessions and idempotency keys, device tombstones whose
// expiry passed, audit events older than 90 days, ended sandboxes older
// than 30 days that no longer reserve a macOS machine identifier,
// processed control events, usage outbox rows 35 days after accounts-api
// accepted them and R2 storage sample records after 1 day. The control
// event job runs only on the lease holder and deletes only events that the
// holder has processed.
package janitor

import (
	"context"
	"log/slog"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// DefaultInterval is the interval of the retention jobs. The retention
// periods are in the job queries: audit events 90 days, ended sandboxes 30
// days, processed control events 1 hour, reported usage outbox rows 35 days
// and storage sample records 1 day.
const DefaultInterval = time.Hour

// ControlCursor reports the delivery ID of the last control event that this
// instance processed as the lease holder. ok is false when the instance does
// not hold the lease or its cursor is not set; the janitor then deletes no
// control event, because an event that no holder processed must stay until a
// holder does. A nil ControlCursor behaves like one that always returns
// false.
type ControlCursor func() (id int64, ok bool)

// controlEventsJob deletes the control events that the lease holder has
// processed (delivery_id up to its cursor) and that are older than the
// retention of 1 hour. $1 is the time of the run and $2 the cursor.
const controlEventsJob = "DELETE FROM control_events WHERE create_time < $1::timestamptz - interval '1 hour' AND delivery_id <= $2"

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
		// Only rows that accounts-api accepted; unreported rows stay until
		// they are reported, also for deleted organizations.
		{"usage_outbox", "DELETE FROM usage_outbox WHERE report_time < $1::timestamptz - interval '35 days'"},
		// A flush receipt only guards the retry of its own batch; 7 days is
		// billing.FlushReceiptRetention (the janitor does not import billing).
		{"usage_flush_receipts", "DELETE FROM usage_flush_receipts WHERE create_time < $1::timestamptz - interval '7 days'"},
		// A sample record only guards the sampling of its own window.
		{"usage_storage_samples", "DELETE FROM usage_storage_samples WHERE window_start < $1::timestamptz - interval '1 day'"},
	}
}

// RunOnce runs every job once at now. The control event job runs only when
// cursor reports a processed cursor; the other jobs run on every instance.
func RunOnce(ctx context.Context, d *db.DB, logger *slog.Logger, now time.Time, cursor ControlCursor) {
	for _, j := range jobs() {
		run(ctx, d, logger, j.name, j.sql, now)
	}
	if cursor == nil {
		return
	}
	id, ok := cursor()
	if !ok {
		return
	}
	run(ctx, d, logger, "control_events", controlEventsJob, now, id)
}

func run(ctx context.Context, d *db.DB, logger *slog.Logger, name, sql string, now time.Time, extra ...any) {
	tag, err := d.Pool.Exec(ctx, sql, append([]any{now}, extra...)...)
	if err != nil {
		logger.WarnContext(ctx, "janitor_job_failed", slog.String("job", name), slog.Any("error", err))
		return
	}
	if n := tag.RowsAffected(); n > 0 {
		logger.InfoContext(ctx, "janitor_job_deleted", slog.String("job", name), slog.Int64("rows", n))
	}
}

// Run runs the jobs every interval until ctx ends.
func Run(ctx context.Context, d *db.DB, logger *slog.Logger, now func() time.Time, interval time.Duration, cursor ControlCursor) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		RunOnce(ctx, d, logger, now(), cursor)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
