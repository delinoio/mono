// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/store"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/vmruntime"
	"github.com/delinoio/mono/internal/backoff"
)

// requestRemoval fences the updater as soon as a channel or session receives
// an authenticated removal notice. Run owns the durable clean-up.
func (a *Agent) requestRemoval() {
	a.removalOnce.Do(func() {
		a.updater.FenceForRemoval()
		close(a.removal)
	})
}

// removeDevice runs the device removal: it records the authenticated
// removal notice, drains the updater, deletes every local sandbox, snapshot
// and image, records the finished clean-up, and deletes the runtime's host
// network resources (see vmruntime.HostCleaner). It confirms removal with a
// signed nonce before deleting the device key. An unknown device means an
// earlier confirmation succeeded.
func (a *Agent) removeDevice(ctx context.Context) (err error) {
	installDone := a.updater.FenceForRemoval()
	// Fence the status file before cleanup: statusLoop is still running when the
	// removal notice arrives through Authenticate or the clean-up command,
	// and a write after Reset would leave status.json behind. A removal
	// that fails keeps the agent enrolled and retries, so it then lifts
	// the fence to keep the status renewing.
	a.stopStatusWriter()
	defer func() {
		if !errors.Is(err, ErrRemoved) {
			a.resumeStatusWriter()
		}
	}()
	st, err := a.st.UpdateAgent(func(s *store.AgentState) error {
		s.Removal.NoticeReceived = true
		return nil
	})
	if err != nil {
		return err
	}
	a.dataPath.Close()
	if installDone != nil {
		select {
		case <-installDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !st.Removal.CleanupDone {
		if err := a.Sandboxes.RemoveAll(ctx); err != nil {
			a.logger.Error("removal clean-up failed", "error", err)
			return err
		}
		if _, err := a.st.UpdateAgent(func(s *store.AgentState) error {
			s.Removal.CleanupDone = true
			return nil
		}); err != nil {
			return err
		}
		a.logger.Info("removal clean-up finished")
	}
	// A removed agent exits for good (the service manager does not restart
	// the not-enrolled exit status), so the host network clean-up of the
	// next agent start never runs: delete the tap devices and the nftables
	// table of the Linux Firecracker runtime now. This is best effort, like
	// the clean-up of the agent start: each failed resource is logged, and
	// `ownsand-agent reset` deletes what is left. It runs also when an
	// earlier process finished the sandbox clean-up, because it is
	// idempotent.
	if hc, ok := a.rt.(vmruntime.HostCleaner); ok {
		if err := hc.CleanupHost(); err != nil {
			a.logger.Warn("removal host network clean-up failed", "error", err)
		} else {
			a.logger.Info("removal host network clean-up finished")
		}
	}
	b := backoff.Backoff{Min: a.opts.BackoffMin, Max: max(a.opts.BackoffMax, a.opts.BackoffMin)}
	for {
		err := a.cp.ConfirmRemoval(ctx, a.DeviceID(), a.key)
		if err == nil || errors.Is(err, cpclient.ErrUnknownDevice) {
			a.logger.Info("removal confirmed", "unknown_device", err != nil)
			break
		}
		delay := b.Next()
		a.logger.Warn("confirm removal failed; retrying", "error", err, "retry_in_ms", delay.Milliseconds())
		if backoff.Sleep(ctx, delay) != nil {
			return ctx.Err()
		}
	}
	if err := a.st.Reset(); err != nil {
		return err
	}
	a.logger.Info("device key deleted; the agent is no longer enrolled")
	return ErrRemoved
}
