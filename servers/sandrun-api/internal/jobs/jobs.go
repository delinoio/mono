// SPDX-License-Identifier: FSL-1.1-ALv2

// Package jobs runs the daily jobs of the lease holder: retention deletes
// (audit events after 90 days in this layer; ended requests and runners after
// 30 days and webhook delivery IDs after 7 days in later layers) and other
// periodic maintenance such as the re-encryption of sealed values after a key
// rotation (see package secrets). The owning contract is the Storage section
// of docs/servers-sandrun-api-foundation.md.
//
// Later layers register jobs with app.Registry.AddDailyJob. Daily is one
// lease component, so the jobs run only on the lease holder: once when the
// instance acquires the lease and then every 24 hours.
package jobs

import (
	"context"
	"log/slog"
	"time"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

// DefaultInterval is the interval of the daily jobs.
const DefaultInterval = 24 * time.Hour

// Job is one daily job. Run returns the number of changed rows for the log
// record. A job must be idempotent: it runs again after every lease
// acquisition.
type Job struct {
	// Name is the stable job name in log records.
	Name string
	Run  func(ctx context.Context, now time.Time) (int64, error)
}

// SQLJob returns a job that runs one statement with the run time as $1, for
// example a retention delete.
func SQLJob(d *db.DB, name, sql string) Job {
	return Job{Name: name, Run: func(ctx context.Context, now time.Time) (int64, error) {
		tag, err := d.Pool.Exec(ctx, sql, now)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	}}
}

// AuditRetention deletes audit events older than 90 days, also those of
// deleted organizations.
func AuditRetention(d *db.DB) Job {
	return SQLJob(d, "audit_events", "DELETE FROM audit_events WHERE create_time < $1::timestamptz - interval '90 days'")
}

// Daily is the lease component that runs the jobs.
type Daily struct {
	Logger   *slog.Logger
	Now      func() time.Time
	Interval time.Duration
	Jobs     []Job
}

// Name returns the component name.
func (d *Daily) Name() string { return "daily_jobs" }

// RunOnce runs every job once at now. A failed job is logged and does not
// stop the others.
func (d *Daily) RunOnce(ctx context.Context, now time.Time) {
	for _, j := range d.Jobs {
		n, err := j.Run(ctx, now)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			d.Logger.WarnContext(ctx, "daily_job_failed", slog.String("job", j.Name), slog.Any("error", err))
			continue
		}
		if n > 0 {
			d.Logger.InfoContext(ctx, "daily_job_done", slog.String("job", j.Name), slog.Int64("rows", n))
		}
	}
}

// Run runs the jobs at once and then every interval until ctx ends.
func (d *Daily) Run(ctx context.Context) error {
	interval := d.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		d.RunOnce(ctx, now().UTC())
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}
