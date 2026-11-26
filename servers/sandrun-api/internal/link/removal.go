// SPDX-License-Identifier: FSL-1.1-ALv2

package link

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/delinoio/mono/servers/sandrun-api/internal/api"
)

const removalDBTimeout = 10 * time.Second

// RemovalOptions configures the link removal lease.
type RemovalOptions struct {
	// ReclaimLegacy permits takeover of marks written before link removal
	// leases existed. Enable it after all such servers and requests drain.
	ReclaimLegacy bool
	// LeaseDuration overrides the production duration in tests.
	LeaseDuration time.Duration
}

// RemovalClaim identifies one teardown attempt. A claim without a link
// records that there was no link to clean up.
type RemovalClaim struct {
	orgID  string
	linkID string
	token  string
}

func (c RemovalClaim) HasLink() bool { return c.linkID != "" }

// ClaimRemoval stops dispatch before an unlink attempt starts cleanup.
func (l *Links) ClaimRemoval(ctx context.Context, orgID string) (RemovalClaim, error) {
	return l.claimRemoval(ctx, orgID, api.NewID())
}

// ClaimOrganizationRemoval uses the authorized organization deletion ID as
// the link lease token. The claim runs on the session that owns the intent.
func (l *Links) ClaimOrganizationRemoval(ctx context.Context, d api.OrganizationDeletion) (RemovalClaim, error) {
	return l.claimRemoval(ctx, d.OrganizationID, d.AttemptID)
}

func (l *Links) claimRemoval(ctx context.Context, orgID, token string) (RemovalClaim, error) {
	if err := ctx.Err(); err != nil {
		return RemovalClaim{}, err
	}
	// Wait for the short database step even after request cancellation. The
	// caller needs the result so it can release a claim that committed.
	claimCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalDBTimeout)
	defer cancel()

	claim := RemovalClaim{orgID: orgID, token: token}
	var wasRemoving, legacy bool
	q := api.OrganizationDeletionQuerier(ctx, l.svc.DB.Pool)
	// Lock before evaluating expiry so a waiter uses the current claim and
	// database time. Keep removing set through the entire takeover.
	err := q.QueryRow(claimCtx, `WITH locked AS MATERIALIZED (
		SELECT ownsand_link_id, removing, removal_token, removal_expires_at
		FROM ownsand_links WHERE organization_id = $1 FOR UPDATE
	), claimed AS (
		UPDATE ownsand_links AS link SET removing = true, removal_token = $2,
			removal_expires_at = clock_timestamp() + $3::interval, organization_deletion_attempt_id = NULL
		FROM locked
		WHERE link.ownsand_link_id = locked.ownsand_link_id
			AND (NOT locked.removing OR locked.removal_expires_at <= clock_timestamp()
				OR ($4 AND locked.removal_token IS NULL))
		RETURNING link.ownsand_link_id
	)
	SELECT COALESCE((SELECT ownsand_link_id::text FROM claimed), ''), removing, removal_token IS NULL FROM locked`,
		orgID, token, l.removalInterval(), l.removal.ReclaimLegacy).Scan(&claim.linkID, &wasRemoving, &legacy)
	if errors.Is(err, pgx.ErrNoRows) {
		return claim, nil
	}
	if err != nil {
		return RemovalClaim{}, err
	}
	if !claim.HasLink() {
		return RemovalClaim{}, api.ErrPrecondition("the OwnSand link is being removed")
	}
	if wasRemoving {
		l.svc.Logger.InfoContext(ctx, "ownsand_link_removal_taken_over", slog.String("organization_id", orgID), slog.Bool("legacy", legacy))
	}
	return claim, nil
}

func (l *Links) removalInterval() pgtype.Interval {
	return pgtype.Interval{Microseconds: l.removal.LeaseDuration.Microseconds(), Valid: true}
}

// WatchRemoval renews the claim until the caller stops watching. A failed
// renewal cancels preparation. At step boundaries the guard also checks the
// database, so preparation cannot continue during the gap before renewal.
func (l *Links) WatchRemoval(ctx context.Context, claim RemovalClaim) (context.Context, func()) {
	if !claim.HasLink() {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancelCause(ctx)
	renewEvery := l.removal.LeaseDuration / 3
	renewCtx, stop := context.WithCancel(ctx)
	fail := func(err error) {
		if ctx.Err() == nil {
			l.svc.Logger.WarnContext(ctx, "ownsand_link_removal_claim_lost",
				slog.String("organization_id", claim.orgID), slog.Any("error", err))
			cancel(err)
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewEvery)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				step, finish := context.WithTimeout(renewCtx, min(renewEvery, removalDBTimeout))
				err := l.renewRemoval(step, claim)
				finish()
				if err != nil && renewCtx.Err() == nil {
					fail(err)
					return
				}
			}
		}
	}()

	guarded := api.WithTeardownCheck(ctx, func(ctx context.Context) error {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		step, finish := context.WithTimeout(ctx, min(renewEvery, removalDBTimeout))
		defer finish()
		var owned int
		err := l.svc.DB.Pool.QueryRow(step, `SELECT 1 FROM ownsand_links
			WHERE ownsand_link_id = $1 AND removal_token = $2 AND removal_expires_at > clock_timestamp()`,
			claim.linkID, claim.token).Scan(&owned)
		if errors.Is(err, pgx.ErrNoRows) {
			err = lostRemoval()
		}
		if err != nil {
			fail(err)
		}
		return err
	})
	return guarded, func() {
		stop()
		<-done
		cancel(nil)
	}
}

func (l *Links) renewRemoval(ctx context.Context, claim RemovalClaim) error {
	tag, err := l.svc.DB.Pool.Exec(ctx, `WITH locked AS MATERIALIZED (
		SELECT removal_expires_at FROM ownsand_links WHERE ownsand_link_id = $1 AND removal_token = $2 FOR UPDATE
	)
	UPDATE ownsand_links SET removal_expires_at = clock_timestamp() + $3::interval
	FROM locked WHERE ownsand_link_id = $1 AND locked.removal_expires_at > clock_timestamp()`,
		claim.linkID, claim.token, l.removalInterval())
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return lostRemoval()
	}
	return nil
}

func lostRemoval() error {
	return api.ErrPrecondition("the OwnSand link removal claim changed or expired; try again")
}

// CheckRemoval locks the link during finalization. The caller already holds
// the organization row lock. An expired claim cannot finalize, and another
// attempt cannot take over after this transaction verifies the claim.
func (l *Links) CheckRemoval(ctx context.Context, tx pgx.Tx, claim RemovalClaim) error {
	var linkID string
	err := tx.QueryRow(ctx, `SELECT ownsand_link_id::text FROM ownsand_links
		WHERE organization_id = $1 FOR UPDATE`, claim.orgID).Scan(&linkID)
	if errors.Is(err, pgx.ErrNoRows) && !claim.HasLink() {
		return nil
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if linkID == "" || linkID != claim.linkID {
		return api.ErrPrecondition("the OwnSand link changed meanwhile; try again")
	}
	var owned bool
	err = tx.QueryRow(ctx, `SELECT COALESCE(removal_token = $2 AND removal_expires_at > clock_timestamp(), false)
		FROM ownsand_links WHERE ownsand_link_id = $1`, claim.linkID, claim.token).Scan(&owned)
	if err != nil {
		return err
	}
	if !owned {
		l.svc.Logger.WarnContext(ctx, "ownsand_link_removal_claim_lost", slog.String("organization_id", claim.orgID))
		return lostRemoval()
	}
	return nil
}

// ReleaseRemoval clears only the caller's unexpired claim. An expired claim
// stays marked so a later attempt can take over without admitting dispatch.
func (l *Links) ReleaseRemoval(ctx context.Context, claim RemovalClaim) {
	if !claim.HasLink() {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), removalDBTimeout)
	defer cancel()
	if _, err := l.svc.DB.Pool.Exec(ctx, `WITH locked AS MATERIALIZED (
		SELECT removal_expires_at FROM ownsand_links WHERE ownsand_link_id = $1 AND removal_token = $2 FOR UPDATE
	)
	UPDATE ownsand_links SET removing = false, removal_token = NULL, removal_expires_at = NULL
	FROM locked WHERE ownsand_link_id = $1 AND locked.removal_expires_at > clock_timestamp()`,
		claim.linkID, claim.token); err != nil {
		l.svc.Logger.WarnContext(ctx, "ownsand_link_removal_unmark_failed",
			slog.String("organization_id", claim.orgID), slog.Any("error", err))
	}
}
