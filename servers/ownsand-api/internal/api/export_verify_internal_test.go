// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestExportVerifierCancelsWhileWaitingForTick(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		ticks := make(chan time.Time)
		defer close(ticks)
		control := &Control{S: &Service{}, ExportVerifyTicks: ticks}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			control.runExportVerifier(ctx)
		}()

		synctest.Wait()
		select {
		case <-done:
			t.Fatal("export verifier exited before cancellation")
		default:
		}

		cancel()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Fatal("export verifier did not exit while waiting for a tick")
		}
	})
}
