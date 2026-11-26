// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
)

const organizationDeletionLockSQL = `SELECT pg_try_advisory_lock(hashtextextended('sandrun.organization-deletion:' || $1::text, 0))`

type organizationDeletionAttempt struct {
	OrganizationDeletion
	conn    *pgx.Conn
	request context.Context
}

type organizationDeletionContextKey struct{}

func (h OrganizationHandler) beginOrganizationDeletion(ctx context.Context, conn *pgx.Conn, orgID string) (OrganizationDeletion, error) {
	var d OrganizationDeletion
	err := pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
		c, err := h.authorizeOwnerAfterLock(ctx, tx, orgID)
		if err != nil {
			return err
		}
		var acquired bool
		if err := tx.QueryRow(ctx, organizationDeletionLockSQL, c.OrganizationID).Scan(&acquired); err != nil {
			return err
		}
		if !acquired {
			return ErrPrecondition("organization deletion is in progress; try again")
		}
		// Closing the session releases its lock. An owner who acquires that
		// lock can replace any intent left behind by the previous session.
		d = OrganizationDeletion{OrganizationID: c.OrganizationID, Actor: c.Principal, AttemptID: NewID()}
		_, err = tx.Exec(ctx, "UPDATE organizations SET deletion_attempt_id = $2 WHERE organization_id = $1", d.OrganizationID, d.AttemptID)
		return err
	})
	return d, err
}

func (a *organizationDeletionAttempt) verify(ctx context.Context, q db.Querier) error {
	var current int
	err := q.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 AND deletion_attempt_id = $2", a.OrganizationID, a.AttemptID).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrPrecondition("organization deletion attempt changed; try again")
	}
	return err
}

// releaseOrganizationDeletion clears only this attempt, even if cancellation
// closed its session. The attempt calls this after cleanup has stopped.
func (s *Service) releaseOrganizationDeletion(ctx context.Context, d OrganizationDeletion) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := s.DB.Pool.Exec(ctx, "UPDATE organizations SET deletion_attempt_id = NULL WHERE organization_id = $1 AND deletion_attempt_id = $2", d.OrganizationID, d.AttemptID); err != nil {
		s.Logger.WarnContext(ctx, "organization_deletion_intent_release_failed", slog.String("organization_id", d.OrganizationID), slog.Any("error", err))
	}
}

type teardownCheckKey struct{}

// WithTeardownCheck attaches a link claim check to the preparation context.
func WithTeardownCheck(ctx context.Context, check func(context.Context) error) context.Context {
	return context.WithValue(ctx, teardownCheckKey{}, check)
}

// CheckTeardown checks the link claim and organization deletion session before
// admitting a cleanup step. A provider call already admitted may have reached
// its peer.
func CheckTeardown(ctx context.Context) error {
	if check, ok := ctx.Value(teardownCheckKey{}).(func(context.Context) error); ok {
		if err := check(ctx); err != nil {
			return err
		}
	}
	a, _ := ctx.Value(organizationDeletionContextKey{}).(*organizationDeletionAttempt)
	if a == nil {
		return nil
	}
	if err := a.request.Err(); err != nil {
		return err
	}
	return a.verify(ctx, a.conn)
}

// OrganizationDeletionQuerier routes cleanup writes through the session
// that owns the deletion lock. A superseded session cannot commit a write.
// Outside organization deletion, the caller's usual querier is used.
func OrganizationDeletionQuerier(ctx context.Context, fallback db.Querier) db.Querier {
	if a, _ := ctx.Value(organizationDeletionContextKey{}).(*organizationDeletionAttempt); a != nil {
		return a.conn
	}
	return fallback
}

// InOrganizationDeletionTx applies the same session fence to cleanup writes
// that need a transaction.
func (s *Service) InOrganizationDeletionTx(ctx context.Context, fn func(pgx.Tx) error) error {
	if a, _ := ctx.Value(organizationDeletionContextKey{}).(*organizationDeletionAttempt); a != nil {
		return pgx.BeginFunc(ctx, a.conn, fn)
	}
	return s.DB.InTx(ctx, fn)
}
