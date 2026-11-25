// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

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
// marked sandboxes that are still PAUSED. The mark is durable billing pause
// ownership: it stays after the resume and while the sandbox is RUNNING,
// until a caller's PauseSandbox takes the ownership or the sandbox ends (see
// resume). It acts only on confirmed statuses: the fail-open rule of stale
// statuses never pauses or resumes a sandbox. Every pass is idempotent and
// restart-safe, because the mark is persisted; a pause or resume that fails
// (for example because the device is offline) is retried by a later pass.
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

// errBillingStale is returned by the send hook of a pause or resume that an
// organization's billing phase made obsolete between the selection of the
// target and the moment the command is sent. The worker drops such work
// quietly.
var errBillingStale = errors.New("the billing phase no longer qualifies the sandbox")

// pausingPhaseSQL returns the SQL condition that an organization_billing row
// (alias b) is in a pausing phase: SUSPENDED, or BLOCKED for at least
// billing.SuspendAfter (the cutoff placeholder holds now minus SuspendAfter).
// The target query of a pass and the recheck right before a pause use this
// one condition, so they cannot disagree. The phase values are constants, not
// input.
func pausingPhaseSQL(cutoffPlaceholder string) string {
	return fmt.Sprintf("(b.phase = %d OR (b.phase = %d AND b.blocked_since <= %s))",
		int16(billing.PhaseSuspended), int16(billing.PhaseBlocked), cutoffPlaceholder)
}

// allowedPhaseSQL is the condition that an organization_billing row (alias
// b) is ALLOWED, shared by the resume target query and the rechecks.
var allowedPhaseSQL = fmt.Sprintf("b.phase = %d", int16(billing.PhaseAllowed))

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
		AND `+pausingPhaseSQL("$2")+`
		ORDER BY s.organization_id, s.sandbox_id`,
		int16(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING), suspendedSince)
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_enforcement_failed", slog.Any("error", err))
		return err
	}
	resume, err := e.targets(ctx, `SELECT s.organization_id::text, s.sandbox_id::text, s.device_id::text, s.agent_state, s.delete_pending
		FROM sandboxes s JOIN organization_billing b ON b.organization_id = s.organization_id
		WHERE s.paused_by_billing AND `+allowedPhaseSQL+`
		ORDER BY s.organization_id, s.sandbox_id`)
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
//
// The selection of the targets ran before this call, and the call can wait
// for a slot while the organization's status changes. The mark therefore
// applies only while the organization is still in a pausing phase, and the
// pause command is sent only after a second check of that phase (see
// pausingPhaseSQL). A recovery that commits after the last check is not
// suppressed; the next pass finds the sandbox marked, RUNNING and ALLOWED and
// leaves it alone (the mark stays, see resume), and a pause that already ran
// is resumed by the next pass.
//
// The mark and the command run under the sandbox ordering lock, so a caller's
// pause of the same sandbox is ordered before the mark or after the command:
// a caller pause that completed first leaves no stale mark on its sandbox
// (the mark applies only to a RUNNING sandbox). A later caller takes ownership
// by adopting an existing pause or successfully queueing its own pause command.
func (e *BillingEnforcer) pause(ctx context.Context, t billingTarget) {
	err := e.s.withSandboxOrder(ctx, t.sandboxID, func(_ *pgx.Conn) error {
		e.pauseOrdered(ctx, t)
		return nil
	})
	if err != nil {
		e.s.Logger.WarnContext(ctx, "billing_sandbox_pause_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.Any("error", err))
	}
}

func (e *BillingEnforcer) pauseOrdered(ctx context.Context, t billingTarget) {
	s := e.s
	cutoff := s.now().Add(-billing.SuspendAfter)
	var eligible, marked bool
	err := s.DB.Pool.QueryRow(ctx, `WITH eligible AS (
			SELECT 1 FROM organization_billing b WHERE b.organization_id = $1 AND `+pausingPhaseSQL("$4")+`
		), upd AS (
			UPDATE sandboxes SET paused_by_billing = true
			WHERE sandbox_id = $2 AND agent_state = $3 AND NOT paused_by_billing AND EXISTS (SELECT 1 FROM eligible)
			RETURNING 1
		)
		SELECT EXISTS (SELECT 1 FROM eligible), EXISTS (SELECT 1 FROM upd)`,
		t.orgID, t.sandboxID, int16(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING), cutoff).Scan(&eligible, &marked)
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_pause_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.Any("error", err))
		return
	}
	if !eligible {
		s.Logger.DebugContext(ctx, "billing_sandbox_pause_stale", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	if marked {
		s.Logger.InfoContext(ctx, "billing_sandbox_marked", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
	}
	if s.Hub.Live(t.deviceID) == nil {
		// The device is offline; a later pass pauses the sandbox after the
		// device reconnects.
		s.Logger.DebugContext(ctx, "billing_sandbox_pause_deferred", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	sb, err := s.pauseOrResumeSandbox(ctx, t.orgID, t.sandboxID, true, pauseOrResumeHooks{beforeSend: func() error {
		return e.recheck(ctx, `SELECT EXISTS (SELECT 1 FROM organization_billing b WHERE b.organization_id = $1 AND `+pausingPhaseSQL("$2")+`)`,
			t.orgID, s.now().Add(-billing.SuspendAfter))
	}})
	if errors.Is(err, errBillingStale) {
		s.Logger.DebugContext(ctx, "billing_sandbox_pause_stale", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_pause_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
			slog.String("device_id", t.deviceID), slog.String("code", connect.CodeOf(err).String()), slog.String("error", err.Error()))
		return
	}
	state, _ := sb.shownState()
	s.Logger.InfoContext(ctx, "billing_sandbox_paused", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
		slog.String("device_id", t.deviceID), slog.String("state", state.String()))
}

// recheck runs a one-row EXISTS query right before a command is sent and
// returns errBillingStale when it is false.
func (e *BillingEnforcer) recheck(ctx context.Context, sql string, args ...any) error {
	var ok bool
	if err := e.s.DB.Pool.QueryRow(ctx, sql, args...).Scan(&ok); err != nil {
		return err
	}
	if !ok {
		return errBillingStale
	}
	return nil
}

// resume resumes a sandbox that billing paused, after a confirmed recovery.
//
// The mark is durable billing pause ownership. Billing keeps it for the rest
// of the sandbox's life, until a caller's PauseSandbox takes the ownership or
// the sandbox ends, and a recovery never clears it for a live sandbox. A
// pause command that billing sent can finish on the agent long after the
// API's wait for it (Timing.CommandWait) ended, after a later pass, a
// resume, a restart of the control plane or the loss of the command record
// (the agent does not cancel a command when its channel closes). The API has
// no record of the commands that are still running, and a RUNNING report
// does not prove that none is. A mark that was cleared on the evidence of a
// RUNNING sandbox or of a successful resume would leave such a late pause
// without an owner, and no recovery would resume it. The mark is cleared only
// for an ended sandbox (DELETED or FAILED), which no command can change any
// more.
//
// Rollout limit: the retention cannot restore a mark that an earlier
// version cleared. A PAUSED sandbox without a mark is indistinguishable from
// a caller's pause, so it is never inferred to belong to billing.
//
// A marked, live sandbox that is not PAUSED is left alone: no command, no
// write and no per-pass log. A marked PAUSED sandbox is resumed only under
// a confirmed ALLOWED status and when it has no pending delete.
//
// Like pause, it rechecks right before the command is sent (and before the
// mark of an ended sandbox is cleared) that the organization is still
// ALLOWED, because the target list can be older than a new suspension. The
// recheck also requires that the sandbox is still marked and PAUSED and has no
// pending delete. A caller's PauseSandbox removes the mark of its sandbox,
// so a target that a caller paused after the selection is dropped by that
// recheck.
func (e *BillingEnforcer) resume(ctx context.Context, t billingTarget) {
	s := e.s
	if t.state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
		if isTerminal(t.state) {
			e.unmarkEnded(ctx, t)
		}
		// A live sandbox that is not PAUSED keeps its mark: a pause that was
		// sent earlier can still complete.
		return
	}
	if t.deletePending {
		// A pending delete ends the sandbox; keep the mark until then.
		return
	}
	if s.Hub.Live(t.deviceID) == nil {
		s.Logger.DebugContext(ctx, "billing_sandbox_resume_deferred", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	// The recheck, the command and the reconciliation of the agent's result run
	// under the sandbox ordering lock. A caller's PauseSandbox of the same
	// sandbox, on any instance, waits for them, and it cancels this resume when
	// it ran first: it removes the mark, which the recheck requires. The mark
	// stays after the resume (see above).
	var sent, resumed bool
	err := s.withSandboxOrder(ctx, t.sandboxID, func(_ *pgx.Conn) error {
		sb, err := s.pauseOrResumeSandbox(ctx, t.orgID, t.sandboxID, false, pauseOrResumeHooks{beforeSend: func() error {
			if err := e.recheck(ctx, `SELECT EXISTS (SELECT 1 FROM sandboxes s JOIN organization_billing b ON b.organization_id = s.organization_id
			WHERE s.sandbox_id = $1 AND s.paused_by_billing AND s.agent_state = $2 AND NOT s.delete_pending AND `+allowedPhaseSQL+`)`,
				t.sandboxID, int16(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)); err != nil {
				return err
			}
			sent = true
			return nil
		}})
		if err != nil {
			return err
		}
		// Only a command that the agent confirmed counts as a resume. When the
		// agent has not reported it yet, a later pass finds the sandbox RUNNING
		// and marked and leaves it alone, or PAUSED and sends the command again.
		resumed = sent && sb.AgentState == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		return nil
	})
	if errors.Is(err, errBillingStale) {
		s.Logger.DebugContext(ctx, "billing_sandbox_resume_stale", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
		return
	}
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_resume_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
			slog.String("device_id", t.deviceID), slog.String("code", connect.CodeOf(err).String()), slog.String("error", err.Error()))
		return
	}
	if resumed {
		s.Logger.InfoContext(ctx, "billing_sandbox_resumed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.String("device_id", t.deviceID))
	}
}

// unmarkEnded clears the mark of an ended sandbox (see resume), after a
// confirmed recovery, and never sends a command. The statement is idempotent
// and rechecks that the organization is still ALLOWED, because the target
// list can be older than a new suspension. Terminal states are absorbing, so
// the agent state of the target is still current.
func (e *BillingEnforcer) unmarkEnded(ctx context.Context, t billingTarget) {
	s := e.s
	tag, err := s.DB.Pool.Exec(ctx, `UPDATE sandboxes SET paused_by_billing = false
		WHERE sandbox_id = $1 AND paused_by_billing AND agent_state IN ($2, $3)
		AND EXISTS (SELECT 1 FROM organization_billing b WHERE b.organization_id = sandboxes.organization_id AND `+allowedPhaseSQL+`)`,
		t.sandboxID, int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED), int16(ownsandv1.SandboxState_SANDBOX_STATE_FAILED))
	if err != nil {
		s.Logger.WarnContext(ctx, "billing_sandbox_resume_failed", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID), slog.Any("error", err))
		return
	}
	if tag.RowsAffected() == 0 {
		// The organization was suspended again, or the sandbox was unmarked,
		// after the target selection.
		s.Logger.DebugContext(ctx, "billing_sandbox_resume_stale", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID))
		return
	}
	s.Logger.InfoContext(ctx, "billing_sandbox_unmarked", slog.String("organization_id", t.orgID), slog.String("sandbox_id", t.sandboxID),
		slog.String("state", t.state.String()))
}
