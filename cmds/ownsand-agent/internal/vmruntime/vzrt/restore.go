// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

func waitGuestResult(ready <-chan error, exited <-chan struct{}) error {
	select {
	case err := <-ready:
		if err != nil {
			return fmt.Errorf("vzrt: guest helper not ready: %w", err)
		}
		return nil
	case <-exited:
		return errors.New("vzrt: the VM stopped during boot")
	}
}

// syncRestoreClock returns the create context error when it ends during the
// clock RPC. A clock failure alone does not invalidate a memory restore.
func syncRestoreClock(ctx context.Context, logger *slog.Logger, sandboxID string, setClock func(context.Context, time.Time) error) error {
	clockCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	clockErr := setClock(clockCtx, time.Now())
	cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	if clockErr != nil {
		logger.Warn("set guest clock after memory restore failed", "sandbox_id", sandboxID, "error", clockErr)
	}
	return nil
}

// finishFailedRestore stops the restore attempt before either discarding it
// or allowing disk fallback. Check the create context after stop because a
// terminal notification can win the readiness wait, and stop can take time.
func finishFailedRestore(ctx context.Context, stop func() error, discard func()) error {
	stopErr := stop()
	if stopErr != nil {
		stopErr = fmt.Errorf("vzrt: stop the VM of the failed memory-state restore: %w", stopErr)
		if err := ctx.Err(); err != nil {
			return errors.Join(err, fmt.Errorf("%w: %w", ErrVMMayRun, stopErr))
		}
		discard()
		return stopErr
	}
	if err := ctx.Err(); err != nil {
		discard()
		return err
	}
	return nil
}
