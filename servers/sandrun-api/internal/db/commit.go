// SPDX-License-Identifier: FSL-1.1-ALv2

package db

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// AfterCommit queues fn to run after the outer transaction commits. Callbacks
// run in registration order and are discarded on rollback or a commit error.
// tx must come from InTx, InTxWithContexts, or one of their savepoints.
func AfterCommit(tx pgx.Tx, fn func()) {
	t := tx.(*commitTx)
	t.afterCommit = append(t.afterCommit, fn)
}

type commitTx struct {
	pgx.Tx
	parent      *commitTx
	afterCommit []func()
}

func (t *commitTx) Begin(ctx context.Context) (pgx.Tx, error) {
	savepoint, err := t.Tx.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &commitTx{Tx: savepoint, parent: t}, nil
}

func (t *commitTx) Commit(ctx context.Context) error {
	callbacks := t.afterCommit
	t.afterCommit = nil
	if err := t.Tx.Commit(ctx); err != nil {
		return err
	}
	if t.parent != nil {
		t.parent.afterCommit = append(t.parent.afterCommit, callbacks...)
		return nil
	}
	for _, fn := range callbacks {
		fn()
	}
	return nil
}

func (t *commitTx) Rollback(ctx context.Context) error {
	t.afterCommit = nil
	return t.Tx.Rollback(ctx)
}
