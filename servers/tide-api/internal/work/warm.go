// SPDX-License-Identifier: FSL-1.1-ALv2

package work

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	createSandboxRejected   = "create_sandbox_rejected"
	maxWarmCreateRejections = 3
	maxWarmCooldown         = 10 * time.Minute
)

var errWarmDeferred = errors.New("warm runner creation deferred")

type warmRetry struct {
	failures   int16
	rejections int16
	retryAfter *time.Time
}

// lockWarmRetry serializes warm admission and outcomes for the profile.
// A new link revision starts with an empty retry state.
func lockWarmRetry(ctx context.Context, tx pgx.Tx, r *Runner) (warmRetry, error) {
	var retry warmRetry
	err := tx.QueryRow(ctx, `SELECT
		CASE WHEN warm_retry_link_revision = $3 THEN warm_failures ELSE 0 END,
		CASE WHEN warm_retry_link_revision = $3 THEN warm_create_rejections ELSE 0 END,
		CASE WHEN warm_retry_link_revision = $3 THEN warm_retry_after END
		FROM runner_profiles WHERE runner_profile_id = $1 AND warm_revision = $2 FOR NO KEY UPDATE`,
		r.ProfileID, r.warmProfileRevision, r.warmLinkRevision).Scan(&retry.failures, &retry.rejections, &retry.retryAfter)
	return retry, err
}

func (e *Engine) allowWarmCreate(ctx context.Context, tx pgx.Tx, r *Runner) error {
	retry, err := lockWarmRetry(ctx, tx, r)
	if errors.Is(err, pgx.ErrNoRows) {
		return errWarmDeferred
	}
	if err != nil {
		return err
	}
	if retry.rejections >= maxWarmCreateRejections || (retry.retryAfter != nil && e.svc.NowUTC().Before(*retry.retryAfter)) {
		return errWarmDeferred
	}
	// Wait for an uncertain CreateSandbox to settle before issuing a new
	// warm create. During recovery, wait for that runner's full start.
	var pending bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM runners WHERE organization_id = $1 AND runner_profile_id = $2
		AND warm_profile_revision = $3 AND warm_link_revision = $4
		AND ((state = $5 AND ownsand_sandbox_id IS NULL) OR ($7 AND state IN ($5, $6)))
	)`, r.OrganizationID, r.ProfileID, r.warmProfileRevision, r.warmLinkRevision, int16(runCreating), int16(runPrep), retry.failures > 0).Scan(&pending)
	if err == nil && pending {
		return errWarmDeferred
	}
	return err
}

// recordWarmFailure runs in the same transaction that fails the runner, so
// the dispatcher cannot see a vacancy without its retry state.
func (e *Engine) recordWarmFailure(ctx context.Context, tx pgx.Tx, r *Runner, rejected bool) (bool, error) {
	retry, err := lockWarmRetry(ctx, tx, r)
	if errors.Is(err, pgx.ErrNoRows) || retry.rejections >= maxWarmCreateRejections {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var currentLink bool
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ownsand_links
		WHERE organization_id = $1 AND credential_revision = $2 AND NOT removing)`, r.OrganizationID, r.warmLinkRevision).Scan(&currentLink)
	if err != nil || !currentLink {
		return false, err
	}
	// Sixty-three failures are enough to reach the cap even with a
	// PassInterval of one nanosecond.
	retry.failures = min(retry.failures+1, 63)
	if rejected {
		retry.rejections++
	} else {
		retry.rejections = 0
	}
	cooldown := min(e.timing.PassInterval, maxWarmCooldown)
	for i := int16(1); i < retry.failures && cooldown < maxWarmCooldown; i++ {
		cooldown += min(cooldown, maxWarmCooldown-cooldown)
	}
	_, err = tx.Exec(ctx, `UPDATE runner_profiles SET warm_retry_link_revision = $2, warm_failures = $3,
		warm_create_rejections = $4, warm_retry_after = $5 WHERE runner_profile_id = $1`,
		r.ProfileID, r.warmLinkRevision, retry.failures, retry.rejections, e.svc.NowUTC().Add(cooldown))
	return retry.rejections == maxWarmCreateRejections, err
}

func resetWarmRetry(ctx context.Context, tx pgx.Tx, r *Runner) error {
	if r.warmProfileRevision == "" || r.RequestID != "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE runner_profiles SET warm_retry_link_revision = NULL, warm_failures = 0,
		warm_create_rejections = 0, warm_retry_after = NULL
		WHERE runner_profile_id = $1 AND warm_revision = $2 AND warm_retry_link_revision = $3
		AND warm_create_rejections < $4`, r.ProfileID, r.warmProfileRevision, r.warmLinkRevision, maxWarmCreateRejections)
	return err
}
