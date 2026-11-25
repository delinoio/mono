// SPDX-License-Identifier: FSL-1.1-ALv2

package vzrt

import (
	"context"
	"errors"
	"fmt"
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
