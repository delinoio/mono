// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"log/slog"
	"time"
)

// DefaultWorkerInterval is the time between two runs of the background
// tasks.
const DefaultWorkerInterval = 30 * time.Second

// workerRunTimeout bounds one run of the tasks. A run is not canceled by the
// shutdown, so a Paddle call or an email that has started finishes and its
// result is recorded; the server waits for it.
const workerRunTimeout = 2 * time.Minute

// workerTask is one background task.
type workerTask struct {
	name string
	run  func(context.Context) error
}

func (s *Service) tasks() []workerTask {
	return []workerTask{
		{"process_auto_top_ups", s.ProcessAutoTopUps},
		{"time_out_top_ups", s.TimeOutTopUps},
		{"expire_grants", s.ExpireGrants},
		{"queue_expiry_notices", s.QueueExpiryNotices},
		{"deliver_notifications", s.DeliverNotifications},
	}
}

// RunOnce runs every background task once. A failed task is logged, and the
// other tasks still run.
func (s *Service) RunOnce(ctx context.Context) {
	for _, task := range s.tasks() {
		if err := task.run(ctx); err != nil {
			if isContextErr(err) && ctx.Err() != nil {
				return
			}
			s.logger.ErrorContext(ctx, "billing_worker_task_failed",
				slog.String("task", task.name), slog.Any("error", err))
		}
	}
}

// RunWorker runs the background tasks every interval, and the automatic
// top-ups at once after Wake, until ctx is done. Several server instances
// may run it at the same time: every task locks what it changes. It returns
// after the run in progress has finished.
func (s *Service) RunWorker(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultWorkerInterval
	}
	s.logger.Info("billing_worker_started", slog.Int64("interval_ms", interval.Milliseconds()))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	run := func(fn func(context.Context)) {
		runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), workerRunTimeout)
		defer cancel()
		fn(runCtx)
	}
	run(s.RunOnce)
	for {
		select {
		case <-ctx.Done():
			s.logger.Info("billing_worker_stopped")
			return
		case <-ticker.C:
			run(s.RunOnce)
		case <-s.wake:
			run(func(ctx context.Context) {
				if err := s.ProcessAutoTopUps(ctx); err != nil {
					s.logger.ErrorContext(ctx, "billing_worker_task_failed",
						slog.String("task", "process_auto_top_ups"), slog.Any("error", err))
				}
			})
		}
	}
}
