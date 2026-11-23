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

// ErrLinkInProgress marks an organization whose link attempt can still be
// running: its LinkOrganization call has not returned or timed out yet.
var ErrLinkInProgress = errors.New("billing: a link attempt is in progress")

// BeginLinkAttemptTx records the link attempt attemptID of the organization
// to billingAccountID in tx, which must hold the organization row lock. The
// organization must have no pending unlink row (the caller sends and
// deletes it first). The attempt counts as running until the call deadline.
func BeginLinkAttemptTx(ctx context.Context, tx pgx.Tx, orgID, attemptID, billingAccountID string, now time.Time) error {
	deadline := now.Add(LinkCallTimeout + linkCallGrace)
	_, err := tx.Exec(ctx, `INSERT INTO billing_pending_unlinks (organization_id, billing_account_id, create_time, attempts, next_attempt_time, link_attempt_id, link_call_deadline)
		VALUES ($1, $2, $3, 0, $4, $5, $4)`, orgID, billingAccountID, now, deadline, attemptID)
	return err
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

// ReleaseLinkAttempt records that the LinkOrganization call of the attempt
// has returned without a confirmed local link: the row stays for the
// reconciliation, which the lease holder starts after the first back-off.
// It changes nothing when another operation took the attempt over.
func (s *Statuses) ReleaseLinkAttempt(ctx context.Context, orgID, attemptID string) error {
	now := s.now()
	_, err := s.db.Pool.Exec(ctx, `UPDATE billing_pending_unlinks SET link_call_deadline = NULL, attempts = 0, next_attempt_time = $3
		WHERE organization_id = $1 AND link_attempt_id = $2`, orgID, attemptID, now.Add(s.unlinkBackoff(1)))
	return err
}

// AbandonLinkAttempt deletes the link attempt after delino-accounts answered
// that it created no link (a definitive failure). It changes nothing when
// another operation took the attempt over.
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
