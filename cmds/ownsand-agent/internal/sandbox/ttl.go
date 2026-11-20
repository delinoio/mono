package sandbox

import (
	"context"
	"time"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/state"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TTLReason returns the end reason of a passed TTL, or UNSPECIFIED when no
// deadline passed. The idle deadline counts only while the idle clock runs.
// When both deadlines passed, the earlier one wins, and MAX_TTL_EXPIRED wins
// a tie.
func TTLReason(now, idleDeadline, maxDeadline time.Time, idleRunning bool) ownsandv1.SandboxEndReason {
	maxPassed := !maxDeadline.IsZero() && !now.Before(maxDeadline)
	idlePassed := idleRunning && !idleDeadline.IsZero() && !now.Before(idleDeadline)
	switch {
	case maxPassed && idlePassed:
		if idleDeadline.Before(maxDeadline) {
			return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
		}
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED
	case maxPassed:
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_MAX_TTL_EXPIRED
	case idlePassed:
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_IDLE_TTL_EXPIRED
	default:
		return ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED
	}
}

// Run enforces TTLs every second and resends unacknowledged updates through
// resend every 15 seconds, until ctx ends.
func (m *Manager) Run(ctx context.Context, resend func([]*ownsandv1.SandboxReport)) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	lastResend := m.cfg.Now()
	lastLost := m.cfg.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.CheckTTL()
			if now := m.cfg.Now(); now.Sub(lastLost) >= 10*time.Second {
				lastLost = now
				m.CheckLost(ctx)
			}
			if now := m.cfg.Now(); resend != nil && now.Sub(lastResend) >= 15*time.Second {
				lastResend = now
				if p := m.PendingUpdates(); len(p) > 0 {
					resend(p)
				}
			}
		}
	}
}

// CheckTTL deletes the sandboxes whose deadline passed and moves the idle
// deadline of sandboxes with running operations.
func (m *Manager) CheckTTL() {
	now := m.cfg.Now()
	m.mu.Lock()
	var expired []struct {
		id     string
		reason ownsandv1.SandboxEndReason
	}
	for id, r := range m.recs.Sandboxes {
		if r.DeleteMarker {
			if !r.MarkerUntil.IsZero() && now.After(r.MarkerUntil) {
				delete(m.recs.Sandboxes, id)
				delete(m.pending, id)
				m.saveLocked()
			}
			continue
		}
		if r.Terminal() || r.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING || m.expiring[id] {
			continue
		}
		running := r.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		if running && m.active[id] > 0 {
			m.touchLocked(r, now)
		}
		reason := TTLReason(now, r.IdleDeadline, r.MaxDeadline, running && m.active[id] == 0)
		if reason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
			m.expiring[id] = true
			expired = append(expired, struct {
				id     string
				reason ownsandv1.SandboxEndReason
			}{id, reason})
		}
	}
	m.mu.Unlock()
	for _, e := range expired {
		m.log.Info("sandbox TTL expired", "sandbox_id", e.id, "end_reason", e.reason.String())
		m.mu.Lock()
		if cancel := m.creates[e.id]; cancel != nil {
			cancel()
		}
		m.mu.Unlock()
		id, reason := e.id, e.reason
		m.enqueue(id, func() {
			if err := m.delete(id, reason); err != nil {
				m.mu.Lock()
				delete(m.expiring, id)
				m.mu.Unlock()
			}
		})
	}
}

// touchLocked restarts the idle clock and reports the new idle deadline at
// most once per IdleReportInterval.
func (m *Manager) touchLocked(r *state.SandboxRecord, now time.Time) {
	next := now.Add(r.IdleTTL)
	if !next.After(r.IdleDeadline) {
		return
	}
	r.IdleDeadline = next
	if now.Sub(m.lastIdleReport[r.SandboxID]) >= IdleReportInterval {
		m.lastIdleReport[r.SandboxID] = now
		m.changeLocked(r, func(*state.SandboxRecord) {})
	}
}

// Touch records sandbox activity (an exec start or end, or a file
// operation): the idle clock restarts.
func (m *Manager) Touch(sandboxID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if r := m.recs.Sandboxes[sandboxID]; r != nil && r.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		m.touchLocked(r, m.cfg.Now())
	}
}

// BeginActivity marks an operation that keeps the sandbox active, such as a
// running exec, until the returned function is called.
func (m *Manager) BeginActivity(sandboxID string) (end func()) {
	m.mu.Lock()
	m.active[sandboxID]++
	if r := m.recs.Sandboxes[sandboxID]; r != nil && r.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
		m.touchLocked(r, m.cfg.Now())
	}
	m.mu.Unlock()
	return func() {
		m.mu.Lock()
		if m.active[sandboxID] > 0 {
			m.active[sandboxID]--
		}
		if r := m.recs.Sandboxes[sandboxID]; r != nil && r.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			m.touchLocked(r, m.cfg.Now())
		}
		m.mu.Unlock()
	}
}

// Recover reconciles the records with the VMs that the runtime still runs
// after an agent start. Deadlines that passed while the agent was stopped
// are enforced at once, and the TTL end reason wins over LOST_ON_DEVICE.
// Every VM is bound to the agent process, so a VM normally does not survive
// a restart; a surviving VM (the binding failed) cannot be re-attached and
// is stopped and deleted. VMs without a record (lost records) are deleted
// too, before the agent reports its new epoch.
func (m *Manager) Recover(ctx context.Context) error {
	vms, err := m.cfg.Runtime.List(ctx)
	if err != nil {
		return err
	}
	live := map[string]bool{}
	for _, v := range vms {
		live[v.SandboxID] = true
	}
	now := m.cfg.Now()
	m.mu.Lock()
	var toDelete []string
	for id := range live {
		if r := m.recs.Sandboxes[id]; r == nil || r.Terminal() {
			toDelete = append(toDelete, id)
		}
	}
	for id, r := range m.recs.Sandboxes {
		if r.Terminal() || r.DeleteMarker {
			continue
		}
		idleRunning := r.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
		reason := TTLReason(now, r.IdleDeadline, r.MaxDeadline, idleRunning)
		ended := ownsandv1.SandboxState_SANDBOX_STATE_DELETED
		if r.State == ownsandv1.SandboxState_SANDBOX_STATE_DELETING && r.PendingEndReason != ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
			reason = r.PendingEndReason
		}
		if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
			ended = ownsandv1.SandboxState_SANDBOX_STATE_FAILED
			reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
		}
		if live[id] {
			toDelete = append(toDelete, id)
		}
		m.log.Info("sandbox ended across an agent restart", "sandbox_id", id, "vm_survived", live[id], "end_reason", reason.String())
		m.changeLocked(r, func(r *state.SandboxRecord) {
			r.State = ended
			r.EndReason = reason
		})
	}
	m.mu.Unlock()
	for _, id := range toDelete {
		if err := m.cfg.Runtime.Delete(ctx, id); err != nil {
			m.log.Error("delete leftover VM failed", "sandbox_id", id, "error", err)
		}
	}
	return nil
}

// DeleteAll stops and deletes every local sandbox, snapshot and locally
// built image and clears the records. Device removal and reset use it.
func (m *Manager) DeleteAll(ctx context.Context) error {
	m.mu.Lock()
	for _, cancel := range m.creates {
		cancel()
	}
	m.mu.Unlock()
	m.gmu.Lock()
	for id, gc := range m.guests {
		_ = gc.Close()
		delete(m.guests, id)
	}
	m.gmu.Unlock()
	if err := m.cfg.Runtime.DeleteAll(ctx); err != nil {
		return err
	}
	m.mu.Lock()
	m.recs.Sandboxes = map[string]*state.SandboxRecord{}
	m.recs.Snapshots = map[string]*state.SnapshotRecord{}
	m.pending = map[string]*ownsandv1.SandboxReport{}
	m.saveLocked()
	m.mu.Unlock()
	return nil
}

// CheckLost reports running and paused sandboxes whose VM no longer exists
// as failed with LOST_ON_DEVICE, or as deleted with the TTL end reason when a
// deadline passed, and releases their resources.
func (m *Manager) CheckLost(ctx context.Context) {
	vms, err := m.cfg.Runtime.List(ctx)
	if err != nil {
		m.log.Warn("list VMs failed", "error", err)
		return
	}
	live := map[string]bool{}
	for _, v := range vms {
		live[v.SandboxID] = true
	}
	now := m.cfg.Now()
	var lost []string
	m.mu.Lock()
	for id, r := range m.recs.Sandboxes {
		if live[id] || m.expiring[id] || (r.State != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && r.State != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED) {
			continue
		}
		reason := TTLReason(now, r.IdleDeadline, r.MaxDeadline, r.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && m.active[id] == 0)
		ended := ownsandv1.SandboxState_SANDBOX_STATE_DELETED
		if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
			ended = ownsandv1.SandboxState_SANDBOX_STATE_FAILED
			reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
		}
		m.log.Warn("sandbox VM lost", "sandbox_id", id, "end_reason", reason.String())
		m.changeLocked(r, func(r *state.SandboxRecord) {
			r.State = ended
			r.EndReason = reason
		})
		lost = append(lost, id)
	}
	m.mu.Unlock()
	for _, id := range lost {
		m.dropGuest(id)
		_ = m.cfg.Runtime.Delete(ctx, id)
	}
}
