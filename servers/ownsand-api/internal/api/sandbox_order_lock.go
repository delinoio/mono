// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// sandboxOrderUnlockTimeout bounds the release of a sandbox ordering lock
// after the work under it ended, also when the request context is done.
const sandboxOrderUnlockTimeout = 5 * time.Second

// sandboxOrderLockSQL takes the Postgres session advisory lock of one
// sandbox. The key is a hash of the sandbox ID, so two sandboxes can share a
// key; that only serializes them, it never lets two holders of one sandbox
// run together.
const sandboxOrderLockSQL = "SELECT pg_advisory_lock(hashtextextended('ownsand.sandbox-order:' || $1::text, 0))"

const sandboxOrderUnlockSQL = "SELECT pg_advisory_unlock(hashtextextended('ownsand.sandbox-order:' || $1::text, 0))"

// sandboxOrderSlots bounds the sandbox ordering locks that this instance
// holds or waits for at the same time. Each one keeps a pooled database
// connection for as long as the work under it runs, which can include the
// command wait. The bound is half of the pool, so the work under a lock
// (and the agent results that it waits for) always finds a free connection.
type sandboxOrderSlots struct {
	once sync.Once
	sem  chan struct{}
}

func (b *sandboxOrderSlots) acquire(ctx context.Context, poolMax int32) error {
	b.once.Do(func() { b.sem = make(chan struct{}, max(1, int(poolMax)/2)) })
	select {
	case b.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *sandboxOrderSlots) release() { <-b.sem }

// orderLockError reports that withSandboxOrder could not take the lock, so fn
// did not run. An error of fn passes through unchanged.
type orderLockError struct{ err error }

func (e *orderLockError) Error() string { return "lock the sandbox ordering: " + e.err.Error() }
func (e *orderLockError) Unwrap() error { return e.err }

// withSandboxOrder runs fn while this caller holds the ordering lock of the
// sandbox. The lock is a Postgres advisory lock, so it orders callers on
// every API instance that uses the database, and a crashed instance
// releases it with its connection.
//
// A caller pause and an automatic billing resume of the same sandbox must
// not interleave: the resume checks that the billing mark still authorizes
// it, sends the command and waits for the agent's result under the lock,
// and the caller pause clears the mark and reads the sandbox state under the
// same lock. A pause that starts while a resume is in flight therefore waits
// until the resume result is reconciled. A resume that starts after a caller
// pause took ownership finds the mark gone and sends nothing.
//
// The callback can use the connection that holds the lock for a short
// transaction. When the lock cannot be released, the connection is closed
// so Postgres releases the lock with the session.
func (s *Service) withSandboxOrder(ctx context.Context, sandboxID string, fn func(*pgx.Conn) error) error {
	if err := s.orderSlots.acquire(ctx, s.DB.Pool.Config().MaxConns); err != nil {
		return &orderLockError{err}
	}
	defer s.orderSlots.release()
	conn, err := s.DB.Pool.Acquire(ctx)
	if err != nil {
		return &orderLockError{err}
	}
	if _, err := conn.Exec(ctx, sandboxOrderLockSQL, sandboxID); err != nil {
		// The lock request may have been granted just before the failure
		// (for example a canceled context), so the session cannot go back
		// to the pool.
		s.discardOrderConn(ctx, sandboxID, conn.Hijack(), err)
		return &orderLockError{err}
	}
	defer func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxOrderUnlockTimeout)
		defer cancel()
		var released bool
		if err := conn.QueryRow(unlockCtx, sandboxOrderUnlockSQL, sandboxID).Scan(&released); err != nil || !released {
			s.discardOrderConn(ctx, sandboxID, conn.Hijack(), err)
			return
		}
		conn.Release()
	}()
	return fn(conn.Conn())
}

// discardOrderConn closes a connection that may still hold a sandbox
// ordering lock, which releases the lock.
func (s *Service) discardOrderConn(ctx context.Context, sandboxID string, conn *pgx.Conn, cause error) {
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sandboxOrderUnlockTimeout)
	defer cancel()
	s.Logger.WarnContext(ctx, "sandbox_order_lock_discarded", slog.String("sandbox_id", sandboxID), slog.Any("error", cause))
	_ = conn.Close(closeCtx)
}
