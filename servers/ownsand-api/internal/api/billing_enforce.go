// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/billing"
)

// DefaultBillingEnforceInterval is the interval of the suspension worker.
const DefaultBillingEnforceInterval = 10 * time.Second

// billingEnforceParallel bounds the pause and resume commands that one pass
// waits for at the same time.
const billingEnforceParallel = 8

// BillingEnforcer is the suspension worker
// (docs/servers-ownsand-api-foundation.md#suspension-and-resume). On the
// lease holder it pauses every RUNNING sandbox of a SUSPENDED organization
// through the PauseSandbox path and marks it as paused by billing, and after
// delino-accounts confirmed the recovery (ALLOWED) it resumes only the
// marked sandboxes that are still PAUSED and clears the mark. It acts only
// on confirmed statuses: the fail-open rule of stale statuses never pauses
// or resumes a sandbox. Every pass is idempotent and restart-safe, because
// the mark is persisted; a pause or resume that fails (for example because
// the device is offline) is retried by a later pass.
type BillingEnforcer struct {
	s        *Service
	interval time.Duration
	kick     chan struct{}
	mu       sync.Mutex
}

// NewBillingEnforcer returns the suspension worker; a zero interval selects
// DefaultBillingEnforceInterval.
func NewBillingEnforcer(s *Service, interval time.Duration) *BillingEnforcer {
	if interval <= 0 {
		interval = DefaultBillingEnforceInterval
	}
	return &BillingEnforcer{s: s, interval: interval, kick: make(chan struct{}, 1)}
}

// Kick runs a pass soon, for example after a status change.
func (e *BillingEnforcer) Kick() {
	select {
	case e.kick <- struct{}{}:
	default:
	}
}

// Run runs passes on the lease holder until ctx ends.
func (e *BillingEnforcer) Run(ctx context.Context) {
	t := time.NewTicker(e.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-e.kick:
		}
		if e.s.Lease != nil && !e.s.Lease.Held() {
			continue
		}
		_ = e.Pass(ctx)
	}
}

// billingTarget is a sandbox that a pass pauses or resumes.
type billingTarget struct {
	orgID, sandboxID, deviceID string
	state                      ownsandv1.SandboxState
	deletePending              bool
}

// Pass runs one suspension and resume pass.
func (e *BillingEnforcer) Pass(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.s
	if s.Billing == nil {
		return nil
	}
	now := s.now()
	suspendedSince := now.Add(-billing.SuspendAfter)
	pause, err := e.targets(ctx, `SELECT s.organization_id::text, s.sandbox_id::text, s.device_id::text, s.agent_state, s.delete_pending
		FROM sandboxes s JOIN organization_billing b ON b.organization_id = s.organization_id
		WHERE s.agent_state = $1 AND NOT s.delete_pending
		AND (b.phase = $2 OR (b.phase = $3 AND b.blocked_since <= $4))
		ORDER BY s.organization_id, s.sandbox_id`,
		int16(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING), int16(billing.PhaseSuspended), int16(billing.PhaseBlocked), suspendedSince)
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_enforcement_failed", slog.Any("error", err))
		return err
	}
	resume, err := e.targets(ctx, `SELECT s.organization_id::text, s.sandbox_id::text, s.device_id::text, s.agent_state, s.delete_pending
		FROM sandboxes s JOIN organization_billing b ON b.organization_id = s.organization_id
		WHERE s.paused_by_billing AND b.phase = $1
		ORDER BY s.organization_id, s.sandbox_id`, int16(billing.PhaseAllowed))
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_enforcement_failed", slog.Any("error", err))
		return err
	}
	sem := make(chan struct{}, billingEnforceParallel)
	var wg sync.WaitGroup
	run := func(fn func()) {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			fn()
		}()
	}
	for _, t := range pause {
		run(func() { e.pause(ctx, t) })
	}
	for _, t := range resume {
		run(func() { e.resume(ctx, t) })
	}
	wg.Wait()
	return nil
}

func (e *BillingEnforcer) targets(ctx context.Context, sql string, args ...any) ([]billingTarget, error) {
	rows, err := e.s.DB.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []billingTarget
	for rows.Next() {
		var t billingTarget
		var state int16
		if err := rows.Scan(&t.orgID, &t.sandboxID, &t.deviceID, &state, &t.deletePending); err != nil {
			return nil, err
		}
		t.state = ownsandv1.SandboxState(state)
		out = append(out, t)
	}
	return out, rows.Err()
}

// pause marks the sandbox as paused by billing and pauses it. The mark is
// written first, so a pause that the agent applied is never left without
// its mark (a crash between the two would leave the sandbox paused after the
// recovery).
func (e *BillingEnforcer) pause(ctx context.Context, t billingTarget) {
	s := e.s
	tag, err := s.DB.Pool.Exec(ctx, "UPDATE sandboxes SET paused_by_billing = true WHERE sandbox_id = $1 AND agent_state = $2 AND NOT paused_by_billing",
		t.sandboxID, int16(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING))
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_pause_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.Any("error", err))
		return
	}
	if tag.RowsAffected() > 0 {
		s.Logger.InfoContext(ctx, "billing_sandbox_marked", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
	}
	if s.Hub.Live(t.deviceID) == nil {
		// The device is offline; a later pass pauses the sandbox after the
		// device reconnects.
		s.Logger.DebugContext(ctx, "billing_sandbox_pause_deferred", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	sb, err := s.pauseOrResumeSandbox(ctx, t.orgID, t.sandboxID, true, func() error { return nil })
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_pause_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
			slog.String("device_id", t.deviceID), slog.String("code", connect.CodeOf(err).String()), slog.String("error", err.Error()))
		return
	}
	state, _ := sb.shownState()
	s.Logger.InfoContext(ctx, "billing_sandbox_paused", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
		slog.String("device_id", t.deviceID), slog.String("state", state.String()))
}

// resume resumes a sandbox that billing paused, after a confirmed recovery,
// and clears its mark. A marked sandbox that is not PAUSED any more (it
// ended, or a pause never happened) only loses its mark.
func (e *BillingEnforcer) resume(ctx context.Context, t billingTarget) {
	s := e.s
	if t.state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED || t.deletePending {
		if t.state == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			// A pending delete ends the sandbox; keep the mark until then.
			return
		}
		if _, err := s.DB.Pool.Exec(ctx, "UPDATE sandboxes SET paused_by_billing = false WHERE sandbox_id = $1 AND paused_by_billing AND agent_state <> $2",
			t.sandboxID, int16(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)); err != nil {
			s.Logger.WarnContext(ctx, "billing_sandbox_resume_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.Any("error", err))
			return
		}
		s.Logger.InfoContext(ctx, "billing_sandbox_unmarked", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
			slog.String("state", t.state.String()))
		return
	}
	if s.Hub.Live(t.deviceID) == nil {
		s.Logger.DebugContext(ctx, "billing_sandbox_resume_deferred", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	sb, err := s.pauseOrResumeSandbox(ctx, t.orgID, t.sandboxID, false, func() error { return nil })
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_resume_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
			slog.String("device_id", t.deviceID), slog.String("code", connect.CodeOf(err).String()), slog.String("error", err.Error()))
		return
	}
	if sb.AgentState != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		// The agent has not reported the resume yet; a later pass checks
		// again and clears the mark then.
		return
	}
	if _, err := s.DB.Pool.Exec(ctx, "UPDATE sandboxes SET paused_by_billing = false WHERE sandbox_id = $1", t.sandboxID); err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_resume_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.Any("error", err))
		return
	}
	s.Logger.InfoContext(ctx, "billing_sandbox_resumed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
}
