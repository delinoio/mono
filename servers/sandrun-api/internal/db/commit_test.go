// SPDX-License-Identifier: FSL-1.1-ALv2

package db_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/sandrun-api/internal/db"
	"github.com/delinoio/mono/servers/sandrun-api/internal/db/dbtest"
)

func TestAfterCommitSavepoints(t *testing.T) {
	d := dbtest.New(t)
	ctx := t.Context()
	conn, err := d.Pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()

	rollback := errors.New("roll back")
	for _, commit := range []bool{false, true} {
		var calls []string
		record := func(tx pgx.Tx, name string) {
			db.AfterCommit(tx, func() { calls = append(calls, name) })
		}
		err := db.InTxWithContexts(ctx, ctx, conn.Conn().Begin, func(tx pgx.Tx) error {
			record(tx, "first")
			if err := pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
				record(savepoint, "rolled back savepoint")
				return rollback
			}); !errors.Is(err, rollback) {
				t.Fatalf("savepoint rollback = %v", err)
			}
			if err := pgx.BeginFunc(ctx, tx, func(savepoint pgx.Tx) error {
				record(savepoint, "released savepoint")
				return nil
			}); err != nil {
				return err
			}
			record(tx, "last")
			if len(calls) != 0 {
				t.Fatalf("callbacks ran before outer commit: %v", calls)
			}
			if !commit {
				return rollback
			}
			return nil
		})
		if commit {
			if err != nil || !slices.Equal(calls, []string{"first", "released savepoint", "last"}) {
				t.Fatalf("outer commit = %v, callbacks = %v", err, calls)
			}
		} else if !errors.Is(err, rollback) || len(calls) != 0 {
			t.Fatalf("outer rollback = %v, callbacks = %v", err, calls)
		}
	}
}
