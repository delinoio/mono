// SPDX-License-Identifier: FSL-1.1-ALv2

package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	ErrOwnSandRemovalDrain  = errors.New("OwnSand removal claim writers did not drain")
	ErrOwnSandRemovalTokens = errors.New("OwnSand removal claims remain")
)

// IsOwnSandTeardownPaused recognizes the admission function's SQLSTATE.
func IsOwnSandTeardownPaused(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "SR001"
}

// CheckOwnSandTeardownAdmission rejects a new RPC during a rollback. The
// claim trigger checks again in the transaction that can write a token.
func (d *DB) CheckOwnSandTeardownAdmission(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, "SELECT check_ownsand_removal_admission()")
	return err
}

// OwnSandTeardownFenced reads the durable fence in the serving database.
func (d *DB) OwnSandTeardownFenced(ctx context.Context) (bool, error) {
	var fenced bool
	err := d.Pool.QueryRow(ctx, "SELECT deployment_id IS NOT NULL FROM ownsand_removal_rollback WHERE singleton").Scan(&fenced)
	return fenced, err
}

// FenceOwnSandTeardown waits for admitted claim transactions to finish. Its
// separate token query sees their commits under READ COMMITTED. A failed
// check rolls back, so it cannot leave a new fence behind.
func (d *DB) FenceOwnSandTeardown(ctx context.Context, deploymentID string) error {
	return d.inOwnSandRollbackTx(ctx, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '10s'"); err != nil {
			return err
		}
		fenced, err := lockOwnSandRollback(ctx, tx)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
				return ErrOwnSandRemovalDrain
			}
			return err
		}
		var tokens bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM ownsand_links WHERE removal_token IS NOT NULL)").Scan(&tokens); err != nil {
			return err
		}
		if tokens {
			return ErrOwnSandRemovalTokens
		}
		if fenced {
			return nil
		}
		_, err = tx.Exec(ctx, "UPDATE ownsand_removal_rollback SET deployment_id = $1 WHERE singleton", deploymentID)
		return err
	})
}

// The token check needs a new snapshot after the lock wait, even when the
// database role or connection settings select a different default isolation.
func (d *DB) inOwnSandRollbackTx(ctx context.Context, fn func(pgx.Tx) error) error {
	return InTxWithContexts(ctx, ctx, func(ctx context.Context) (pgx.Tx, error) {
		return d.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	}, fn)
}

func lockOwnSandRollback(ctx context.Context, tx pgx.Tx) (bool, error) {
	var fenced bool
	err := tx.QueryRow(ctx, "SELECT deployment_id IS NOT NULL FROM ownsand_removal_rollback WHERE singleton FOR UPDATE").Scan(&fenced)
	return fenced, err
}

// CancelOwnSandRollback reopens admission only if this attempt closed it.
// Lock the row first so a claim check with an unconfirmed reply finishes
// its commit or rollback before cancel tests the deployment ID.
func (d *DB) CancelOwnSandRollback(ctx context.Context, deploymentID string) error {
	return d.inOwnSandRollbackTx(ctx, func(tx pgx.Tx) error {
		fenced, err := lockOwnSandRollback(ctx, tx)
		if err != nil || !fenced {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE ownsand_removal_rollback SET deployment_id = NULL WHERE singleton AND deployment_id = $1", deploymentID)
		return err
	})
}

// ResumeOwnSandTeardown reopens admission after a confirmed deployment of
// a compatible image and retirement of the previous containers.
func (d *DB) ResumeOwnSandTeardown(ctx context.Context) error {
	_, err := d.Pool.Exec(ctx, "UPDATE ownsand_removal_rollback SET deployment_id = NULL WHERE singleton AND deployment_id IS NOT NULL")
	return err
}
