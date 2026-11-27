// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// ErrOwnerUnlinkSuperseded means the owner request's intent no longer
// applies to the current link.
var ErrOwnerUnlinkSuperseded = errors.New("billing: the owner unlink was superseded")

type ownerUnlink struct {
	id          string
	accountID   string
	attempts    int
	nextAttempt time.Time
}

func loadOwnerUnlink(ctx context.Context, q db.Querier, orgID string) (ownerUnlink, error) {
	var op ownerUnlink
	err := q.QueryRow(ctx, `SELECT unlink_id::text, billing_account_id::text, attempts, next_attempt_time
		FROM billing_owner_unlinks WHERE organization_id = $1`, orgID).Scan(&op.id, &op.accountID, &op.attempts, &op.nextAttempt)
	if errors.Is(err, pgx.ErrNoRows) {
		err = nil
	}
	return op, err
}

// PrepareOwnerUnlinkTx reuses an existing intent or records a new one for
// the owner's current link. The caller must hold the organization lock and
// commit before sending the unlink. An empty ID selects the legacy path
// during rollout, when creation of new intents is disabled.
func (s *Statuses) PrepareOwnerUnlinkTx(ctx context.Context, tx pgx.Tx, orgID, accountID, id string) (string, error) {
	op, err := loadOwnerUnlink(ctx, tx, orgID)
	if err != nil || op.id != "" || !s.writeOwnerUnlinks {
		return op.id, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO billing_owner_unlinks (organization_id, billing_account_id, unlink_id, next_attempt_time)
		VALUES ($1, $2, $3, $4)`, orgID, accountID, id, s.now())
	return id, err
}

// ResolveOwnerUnlinkTx finishes an intent under the organization lock. A link
// request passes an empty expectedID to resolve any pending owner unlink.
// An unlink request passes its own ID so a delayed request cannot affect
// work that a later owner request authorized.
func (s *Statuses) ResolveOwnerUnlinkTx(ctx context.Context, tx pgx.Tx, orgID, expectedID string) (Status, Change, error) {
	op, err := loadOwnerUnlink(ctx, tx, orgID)
	if err != nil {
		return Status{}, Change{}, err
	}
	return s.resolveOwnerUnlink(ctx, tx, orgID, op, expectedID)
}

func (s *Statuses) resolveOwnerUnlink(ctx context.Context, tx pgx.Tx, orgID string, op ownerUnlink, expectedID string) (Status, Change, error) {
	st, ok, err := LoadStatus(ctx, tx, orgID, false)
	if err != nil {
		return Status{}, Change{}, err
	}
	if !ok {
		return Status{}, Change{}, pgx.ErrNoRows
	}
	if expectedID != "" && op.id != expectedID && st.Linked() {
		return Status{}, Change{}, ErrOwnerUnlinkSuperseded
	}
	if op.id == "" {
		return st, Change{}, nil
	}
	if st.BillingAccountID != op.accountID {
		_, err := tx.Exec(ctx, "DELETE FROM billing_owner_unlinks WHERE organization_id = $1", orgID)
		return st, Change{}, err
	}

	var guarded bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM billing_pending_unlinks WHERE organization_id = $1
		AND (link_call_deadline > $2 OR unlink_not_before > $2))`, orgID, s.now()).Scan(&guarded); err != nil {
		return Status{}, Change{}, err
	}
	if guarded {
		return Status{}, Change{}, ErrLinkInProgress
	}

	if callErr := s.callUnlink(ctx, orgID); callErr != nil {
		next := s.now().Add(s.unlinkBackoff(op.attempts + 1))
		if _, err := tx.Exec(ctx, "UPDATE billing_owner_unlinks SET attempts = attempts + 1, next_attempt_time = $2 WHERE organization_id = $1", orgID, next); err != nil {
			return Status{}, Change{}, err
		}
		s.logUnlinkFailure(ctx, orgID, op.accountID, op.attempts+1, next, callErr)
		return Status{}, Change{}, errors.Join(ErrUnlinkFailed, callErr)
	}
	// Accounts confirmed removal, so an earlier cleanup row can be deleted.
	if _, err := tx.Exec(ctx, "DELETE FROM billing_pending_unlinks WHERE organization_id = $1", orgID); err != nil {
		return Status{}, Change{}, err
	}
	// The status update clears the intent in the same transaction. A failed
	// commit leaves both the cached link and the intent available to retry.
	st, _, change, err := UnlinkTx(ctx, tx, orgID, SourceUnlink, s.now())
	return st, change, err
}
