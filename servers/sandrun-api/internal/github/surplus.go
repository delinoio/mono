// SPDX-License-Identifier: FSL-1.1-ALv2

package github

import (
	"context"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	sandrunv1 "github.com/delinoio/mono/protos/delino/sandrun/v1"
)

const (
	surplusBatchSize    = 16
	surplusCheckTimeout = 30 * time.Second
)

type surplusCheck struct {
	installationID string
	profileID      string
	organizationID string
	generation     string
}

// runSurplusChecks checks for work on lease acquisition and while held.
func (l *Layer) runSurplusChecks(ctx context.Context) error {
	t := time.NewTicker(l.timing.SurplusInterval)
	defer t.Stop()
	for {
		l.CheckSurplusOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// CheckSurplusOnce processes a bounded batch of due pool checks. Only the
// lease component calls it in production.
func (l *Layer) CheckSurplusOnce(ctx context.Context) {
	rows, err := l.svc.DB.Pool.Query(ctx, `SELECT installation_id::text, runner_profile_id::text, organization_id::text, generation::text
		FROM github_surplus_checks WHERE check_after <= now() ORDER BY check_after, generation LIMIT $1`, surplusBatchSize)
	if err == nil {
		var checks []surplusCheck
		checks, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (surplusCheck, error) {
			var check surplusCheck
			err := row.Scan(&check.installationID, &check.profileID, &check.organizationID, &check.generation)
			return check, err
		})
		if err == nil {
			for _, check := range checks {
				if ctx.Err() != nil {
					return
				}
				l.processSurplusCheck(ctx, check)
			}
			return
		}
	}
	if ctx.Err() == nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_list_failed", slog.Any("error", err))
	}
}

func (l *Layer) processSurplusCheck(ctx context.Context, check surplusCheck) {
	checkCtx, cancel := context.WithTimeout(ctx, surplusCheckTimeout)
	complete := l.removeSurplus(checkCtx, check) && checkCtx.Err() == nil
	cancel()

	// A completion can replace the generation while provider I/O runs.
	// Neither acknowledgment nor delay may change that newer check.
	const key = " WHERE installation_id = $1 AND runner_profile_id = $2 AND generation = $3"
	args := []any{check.installationID, check.profileID, check.generation}
	query := "DELETE FROM github_surplus_checks" + key
	if !complete {
		// Move a failing pool past other due pools so it cannot monopolize
		// the batch. A new completion makes the check due immediately.
		query = "UPDATE github_surplus_checks SET check_after = now() + $4::bigint * interval '1 microsecond'" + key
		args = append(args, l.timing.SurplusInterval.Microseconds())
	}
	if _, err := l.svc.DB.Pool.Exec(ctx, query, args...); err != nil && ctx.Err() == nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_update_failed", slog.String("organization_id", check.organizationID),
			slog.String("runner_profile_id", check.profileID), slog.Any("error", err))
	}
}

// removeSurplus returns true when the pool has no more safe surplus to
// process, or its profile or active installation no longer exists.
func (l *Layer) removeSurplus(ctx context.Context, check surplusCheck) bool {
	orgID, profileID := check.organizationID, check.profileID
	inst, err := l.installation(ctx, l.svc.DB.Pool, orgID, check.installationID)
	if connect.CodeOf(err) == connect.CodeNotFound || (err == nil && !inst.Active()) {
		return true
	}
	if err != nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		return false
	}

	pool := inst.PoolKey()
	// Retire unusable labels before counting the capacity that can still
	// take queued jobs. Queued events can also retire renamed runners.
	l.retireRenamed(ctx, orgID, profileID, pool)
	p, err := l.profiles.Get(ctx, l.svc.DB.Pool, orgID, profileID)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return true
	}
	if err != nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_profile_read_failed",
			slog.String("organization_id", orgID), slog.String("runner_profile_id", profileID), slog.Any("error", err))
		return false
	}
	surplus, err := l.engine.PoolSurplus(ctx, orgID, profileID, pool, int(p.WarmCount))
	if err != nil {
		l.svc.Logger.WarnContext(ctx, "github_surplus_failed", slog.String("organization_id", orgID), slog.Any("error", err))
		return false
	}
	complete := true
	for _, r := range surplus {
		if ctx.Err() != nil {
			return false
		}
		deleted, err := l.removeIdleRunner(ctx, inst, orgID, r.ID, sandrunv1.RunnerEndReason_RUNNER_END_REASON_SURPLUS)
		if err != nil {
			l.svc.Logger.WarnContext(ctx, "github_surplus_delete_failed", slog.String("organization_id", orgID), slog.String("runner_id", r.ID), slog.Any("error", err))
			complete = false
			continue
		}
		if deleted {
			l.svc.Logger.InfoContext(ctx, "github_surplus_runner_removed", slog.String("organization_id", orgID), slog.String("runner_id", r.ID))
		}
	}
	return complete
}
