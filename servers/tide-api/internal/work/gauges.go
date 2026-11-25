// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	tidev1 "github.com/delinoio/mono/protos/delino/tide/v1"
)

const (
	requestGaugeQuery = "SELECT kind, priority, state, count(*) FROM requests WHERE state = ANY($1) GROUP BY kind, priority, state"
	runnerGaugeQuery  = "SELECT kind, guest_os, state, count(*) FROM runners WHERE state = ANY($1) GROUP BY kind, guest_os, state"
)

type gaugeCounts map[[3]string]int64

type workGaugeSnapshot struct {
	requests gaugeCounts
	runners  gaugeCounts
}

type gaugeQuery func(context.Context, string, ...any) (pgx.Rows, error)

// RunGauges samples tide_requests and tide_runners while the lease is held.
func (e *Engine) RunGauges(ctx context.Context) error {
	var published atomic.Pointer[workGaugeSnapshot]
	var current workGaugeSnapshot
	stopObserving := e.m.ObserveWorkGauges(ctx, func() (requests, runners map[[3]string]int64) {
		snapshot := published.Load()
		if snapshot == nil {
			return nil, nil
		}
		return snapshot.requests, snapshot.runners
	})
	defer stopObserving()

	t := time.NewTicker(e.timing.GaugeInterval)
	defer t.Stop()
	for ctx.Err() == nil {
		sample := sampleWorkGauges(ctx, e.svc.DB.Pool.Query, e.svc.Logger, current)
		if err := ctx.Err(); err != nil {
			return err
		}
		next := current.withSample(sample)
		published.Store(&next)
		current = next
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
	return ctx.Err()
}

// withSample keeps the last complete counts for any failed query.
func (previous workGaugeSnapshot) withSample(sample workGaugeSnapshot) workGaugeSnapshot {
	if sample.requests == nil {
		sample.requests = previous.requests
	}
	if sample.runners == nil {
		sample.runners = previous.runners
	}
	return sample
}

func sampleWorkGauges(ctx context.Context, query gaugeQuery, logger *slog.Logger, previous workGaugeSnapshot) workGaugeSnapshot {
	requests, err := sampleGauge(ctx, query, requestGaugeQuery, openRequestStates, previous.requests, func(kind, priority, state int16) [3]string {
		return [3]string{kindLabel(tidev1.WorkloadKind(kind)), priorityLabel(tidev1.Priority(priority)), reqStateLabel(tidev1.RequestState(state))}
	})
	if err != nil && ctx.Err() == nil {
		logger.WarnContext(ctx, "work_gauge_sample_failed", slog.String("gauge", "tide_requests"), slog.Any("error", err))
	}
	runners, err := sampleGauge(ctx, query, runnerGaugeQuery, openRunnerStates, previous.runners, func(kind, guest, state int16) [3]string {
		return [3]string{kindLabel(tidev1.WorkloadKind(kind)), guestLabel(tidev1.GuestOs(guest)), runStateLabel(tidev1.RunnerState(state))}
	})
	if err != nil && ctx.Err() == nil {
		logger.WarnContext(ctx, "work_gauge_sample_failed", slog.String("gauge", "tide_runners"), slog.Any("error", err))
	}
	return workGaugeSnapshot{requests: requests, runners: runners}
}

func sampleGauge(ctx context.Context, query gaugeQuery, statement string, states []int16, previous gaugeCounts, labels func(int16, int16, int16) [3]string) (gaugeCounts, error) {
	rows, err := query(ctx, statement, states)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(gaugeCounts, len(previous))
	for rows.Next() {
		var kind, dimension, state int16
		var count int64
		if err := rows.Scan(&kind, &dimension, &state, &count); err != nil {
			return nil, err
		}
		counts[labels(kind, dimension, state)] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for group := range previous {
		if _, ok := counts[group]; !ok {
			counts[group] = 0
		}
	}
	return counts, nil
}
