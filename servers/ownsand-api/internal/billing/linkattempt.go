// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// Link attempts (docs/servers-ownsand-api-foundation.md#billing-links).
//
// LinkBillingAccount calls LinkOrganization outside the local transaction
// that records the link, so a lost response or a failed local commit could
// leave a remote link without a local trace. A link attempt is the durable
// trace: a billing_pending_unlinks row with a link attempt ID that is
// committed before the call and removed in the transaction that records the
// link. While the row exists, the unlink machinery of the pending unlinks
// (the retry of the lease holder, UnlinkBillingAccount, TransferOwnership
// and DeleteOrganization) removes the remote link that the call may have
// created.

const (
	// LinkCallTimeout bounds one LinkOrganization call of a link attempt.
	LinkCallTimeout = 15 * time.Second
	// linkCallGrace is added to LinkCallTimeout for the deadline of the call:
	// the time that delino-accounts can still need after the client gave up.
	linkCallGrace = 15 * time.Second
)

// ErrLinkInProgress marks an organization whose LinkOrganization call can
// still be running or can still take effect in Accounts.
var ErrLinkInProgress = errors.New("billing: a link attempt is in progress")

// BeginLinkAttemptTx records the link attempt attemptID of the organization
// to billingAccountID in tx, which must hold the organization row lock. The
// organization must have no pending unlink row (the caller sends and
// deletes it first). The attempt counts as running until the call deadline,
// which it returns: LinkCallBudget admits the call only while it can end
// before that deadline, so the cleanup cannot overtake it.
func BeginLinkAttemptTx(ctx context.Context, tx pgx.Tx, orgID, attemptID, billingAccountID string, now time.Time) (time.Time, error) {
	// Use the same precision as the persisted Postgres deadline.
	deadline := now.Add(LinkCallTimeout + linkCallGrace).Truncate(time.Microsecond)
	_, err := tx.Exec(ctx, `INSERT INTO billing_pending_unlinks (organization_id, billing_account_id, create_time, attempts, next_attempt_time, link_attempt_id, link_call_deadline)
		VALUES ($1, $2, $3, 0, $4, $5, $4)`, orgID, billingAccountID, now, deadline, attemptID)
	return deadline, err
}

// LinkCallBudget returns how long the LinkOrganization call of an attempt
// with the persisted call deadline may run when it is dispatched at now: at
// most LinkCallTimeout, and short enough that the call and the time that
// delino-accounts can still need after the client gave up end before the
// deadline, after which a cleanup may remove the attempt row. A budget of
// zero or less means the call must not be started: the attempt is too old
// for a call to end before its row can be removed.
func LinkCallBudget(deadline, now time.Time) time.Duration {
	return min(LinkCallTimeout, deadline.Add(-linkCallGrace).Sub(now))
}

// LinkAttemptRunning reports whether the row of the organization still
// carries the attempt attemptID with a running call: no unlink, ownership
// transfer, deletion, newer attempt or cleanup took it over or removed it.
// A call must not be started for an attempt that is not running.
func LinkAttemptRunning(ctx context.Context, q db.Querier, orgID, attemptID string) (bool, error) {
	var running bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_pending_unlinks
		WHERE organization_id = $1 AND link_attempt_id = $2 AND link_call_deadline IS NOT NULL)`, orgID, attemptID).Scan(&running)
	return running, err
}

// CompleteLinkAttemptTx removes the link attempt from tx, which must hold the
// organization row lock. It reports false when the row no longer carries
// attemptID: another operation (an unlink, an ownership transfer, a
// deletion or a newer attempt) took the attempt over, and the caller must
// not record the link.
func CompleteLinkAttemptTx(ctx context.Context, tx pgx.Tx, orgID, attemptID string) (bool, error) {
	tag, err := tx.Exec(ctx, "DELETE FROM billing_pending_unlinks WHERE organization_id = $1 AND link_attempt_id = $2", orgID, attemptID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseLinkAttempt clears the deadline when dispatch did not start or
// Accounts confirmed the link but the local write failed. The row stays
// for reconciliation after the first back-off. It changes nothing when
// another operation took the attempt over.
func (s *Statuses) ReleaseLinkAttempt(ctx context.Context, orgID, attemptID string) error {
	now := s.now()
	_, err := s.db.Pool.Exec(ctx, `UPDATE billing_pending_unlinks SET link_call_deadline = NULL, attempts = 0, next_attempt_time = $3
		WHERE organization_id = $1 AND link_attempt_id = $2`, orgID, attemptID, now.Add(s.unlinkBackoff(1)))
	return err
}

// AbandonLinkAttempt deletes the matching attempt if dispatch did not start
// or delino-accounts definitively rejected the call.
func (s *Statuses) AbandonLinkAttempt(ctx context.Context, q db.Querier, orgID, attemptID string) error {
	_, err := q.Exec(ctx, "DELETE FROM billing_pending_unlinks WHERE organization_id = $1 AND link_attempt_id = $2", orgID, attemptID)
	return err
}

// TakeOverLinkAttemptTx turns the link attempt of the organization into a
// plain pending unlink in tx, which must hold the organization row lock. The
// unlink is not sent before the call of the attempt has returned or timed
// out, so it cannot overtake the call: the call deadline moves to
// unlink_not_before, which every send of the row honours (the retry of the
// lease holder and the synchronous link and unlink RPCs alike), and to
// next_attempt_time. It reports whether an attempt existed. A completion of
// the old attempt finds no row with its ID and records no link.
func TakeOverLinkAttemptTx(ctx context.Context, tx pgx.Tx, orgID string, now time.Time) (bool, error) {
	tag, err := tx.Exec(ctx, `UPDATE billing_pending_unlinks SET link_attempt_id = NULL, link_call_deadline = NULL, attempts = 0,
		unlink_not_before = greatest($2::timestamptz, COALESCE(link_call_deadline, $2::timestamptz)),
		next_attempt_time = greatest($2::timestamptz, COALESCE(link_call_deadline, $2::timestamptz))
		WHERE organization_id = $1 AND link_attempt_id IS NOT NULL`, orgID, now)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
