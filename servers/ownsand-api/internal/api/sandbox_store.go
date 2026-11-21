// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/types/known/durationpb"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// sandboxRow is a stored sandbox with the state of its device.
type sandboxRow struct {
	ID, OrgID, DeviceID string
	AgentState          ownsandv1.SandboxState
	EndReason           ownsandv1.SandboxEndReason
	Epoch               string
	Counter             uint64
	DeletePending       bool
	DeleteEndReason     ownsandv1.SandboxEndReason
	DeleteActor         auth.Principal
	SourceBaseImage     ownsandv1.BaseImage
	SourceSnapshotID    string
	SourceTemplateID    string
	ResolvedSnapshotID  string
	VCPUCount           int32
	MemoryMiB, DiskGiB  int64
	GuestOS             ownsandv1.GuestOs
	HostOS              ownsandv1.HostOs
	Arch                ownsandv1.Arch
	NetworkMode         ownsandv1.NetworkMode
	Labels              map[string]string
	IdleTTL, MaxTTL     time.Duration
	IdleDeadline        *time.Time
	MaxDeadline         time.Time
	RestorePath         ownsandv1.SandboxRestorePath
	MachineLineage      string
	CreatedBy           auth.Principal
	CreateTime          time.Time
	EndTime             *time.Time
	DeviceState         ownsandv1.DeviceState
}

const sandboxSelect = `SELECT s.sandbox_id::text, s.organization_id::text, s.device_id::text, s.agent_state, COALESCE(s.end_reason, 0),
	s.revision_epoch, s.revision_counter, s.delete_pending, COALESCE(s.delete_end_reason, 0), COALESCE(s.delete_actor_kind, 0), COALESCE(s.delete_actor_id, ''),
	COALESCE(s.source_base_image, 0), COALESCE(s.source_snapshot_id::text, ''), COALESCE(s.source_template_id::text, ''), COALESCE(s.resolved_snapshot_id::text, ''),
	s.vcpu_count, s.memory_mib, s.disk_gib, s.guest_os, s.host_os, s.arch, s.network_mode, s.labels,
	s.idle_ttl_ms, s.max_ttl_ms, s.idle_deadline, s.max_deadline, s.restore_path, COALESCE(s.machine_lineage::text, ''),
	s.created_by_kind, s.created_by_id, s.create_time, s.end_time, COALESCE(d.state, 3)
	FROM sandboxes s LEFT JOIN devices d ON d.device_id = s.device_id`

func scanSandbox(row pgx.Row) (*sandboxRow, error) {
	var r sandboxRow
	var agentState, endReason, delReason, delKind, base, guest, host, arch, network, restore, createdKind, devState int16
	var counter int64
	var labels []byte
	var idleMs, maxMs int64
	if err := row.Scan(&r.ID, &r.OrgID, &r.DeviceID, &agentState, &endReason, &r.Epoch, &counter, &r.DeletePending, &delReason, &delKind, &r.DeleteActor.ID,
		&base, &r.SourceSnapshotID, &r.SourceTemplateID, &r.ResolvedSnapshotID, &r.VCPUCount, &r.MemoryMiB, &r.DiskGiB, &guest, &host, &arch, &network, &labels,
		&idleMs, &maxMs, &r.IdleDeadline, &r.MaxDeadline, &restore, &r.MachineLineage, &createdKind, &r.CreatedBy.ID, &r.CreateTime, &r.EndTime, &devState); err != nil {
		return nil, err
	}
	r.AgentState = ownsandv1.SandboxState(agentState)
	r.EndReason = ownsandv1.SandboxEndReason(endReason)
	r.Counter = uint64(counter)
	r.DeleteEndReason = ownsandv1.SandboxEndReason(delReason)
	r.DeleteActor.Kind = ownsandv1.PrincipalKind(delKind)
	r.SourceBaseImage = ownsandv1.BaseImage(base)
	r.GuestOS = ownsandv1.GuestOs(guest)
	r.HostOS = ownsandv1.HostOs(host)
	r.Arch = ownsandv1.Arch(arch)
	r.NetworkMode = ownsandv1.NetworkMode(network)
	r.IdleTTL = time.Duration(idleMs) * time.Millisecond
	r.MaxTTL = time.Duration(maxMs) * time.Millisecond
	r.RestorePath = ownsandv1.SandboxRestorePath(restore)
	r.CreatedBy.Kind = ownsandv1.PrincipalKind(createdKind)
	r.DeviceState = ownsandv1.DeviceState(devState)
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &r.Labels); err != nil {
			return nil, err
		}
	}
	return &r, nil
}

// loadSandbox reads one sandbox of the organization; lock adds FOR UPDATE
// OF the sandbox row.
func loadSandbox(ctx context.Context, q db.Querier, orgID, sandboxID string, lock bool) (*sandboxRow, error) {
	sql := sandboxSelect + " WHERE s.organization_id = $1 AND s.sandbox_id = $2"
	if lock {
		sql += " FOR UPDATE OF s"
	}
	r, err := scanSandbox(q.QueryRow(ctx, sql, orgID, sandboxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("sandbox")
	}
	return r, err
}

func isTerminal(state ownsandv1.SandboxState) bool {
	return state == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || state == ownsandv1.SandboxState_SANDBOX_STATE_FAILED
}

// shownState returns the state that the API reports and, for an
// UNREACHABLE sandbox, its last known state. A pending delete shows as
// DELETING; a non-terminal sandbox on a device that is not online shows as
// UNREACHABLE.
func (r *sandboxRow) shownState() (ownsandv1.SandboxState, ownsandv1.SandboxState) {
	if isTerminal(r.AgentState) {
		return r.AgentState, 0
	}
	state := r.AgentState
	if r.DeletePending {
		state = ownsandv1.SandboxState_SANDBOX_STATE_DELETING
	}
	if r.DeviceState != ownsandv1.DeviceState_DEVICE_STATE_ONLINE {
		return ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE, state
	}
	return state, 0
}

func (r *sandboxRow) proto() *ownsandv1.Sandbox {
	state, last := r.shownState()
	sb := &ownsandv1.Sandbox{
		SandboxId:        r.ID,
		OrganizationId:   r.OrgID,
		DeviceId:         r.DeviceID,
		State:            state,
		SourceSnapshotId: r.ResolvedSnapshotID,
		Resources:        &ownsandv1.Resources{VcpuCount: r.VCPUCount, MemoryMib: r.MemoryMiB, DiskGib: r.DiskGiB},
		GuestOs:          r.GuestOS,
		HostOs:           r.HostOS,
		Arch:             r.Arch,
		Network:          &ownsandv1.Network{Mode: r.NetworkMode},
		Labels:           r.Labels,
		IdleTtl:          durationpb.New(r.IdleTTL),
		MaxTtl:           durationpb.New(r.MaxTTL),
		IdleDeadline:     tsPtr(r.IdleDeadline),
		MaxDeadline:      ts(r.MaxDeadline),
		RestorePath:      r.RestorePath,
		CreatedBy:        r.CreatedBy.Proto(),
		CreateTime:       ts(r.CreateTime),
		EndTime:          tsPtr(r.EndTime),
	}
	if last != 0 {
		sb.LastKnownState = &last
	}
	if isTerminal(state) && r.EndReason != 0 {
		reason := r.EndReason
		sb.EndReason = &reason
	}
	switch {
	case r.SourceBaseImage != 0:
		sb.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_BaseImage{BaseImage: r.SourceBaseImage}}
	case r.SourceTemplateID != "":
		sb.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_TemplateId{TemplateId: r.SourceTemplateID}}
	case r.SourceSnapshotID != "":
		sb.Source = &ownsandv1.SandboxSource{Source: &ownsandv1.SandboxSource_SnapshotId{SnapshotId: r.SourceSnapshotID}}
	}
	return sb
}

// endSandboxTx moves one non-terminal sandbox to a terminal state, writes
// its SANDBOX_DELETED audit event and its revocation event.
func (s *Service) endSandboxTx(ctx context.Context, tx pgx.Tx, r *sandboxRow, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason, actor auth.Principal) error {
	if isTerminal(r.AgentState) {
		return nil
	}
	now := s.now()
	if _, err := tx.Exec(ctx, `UPDATE sandboxes SET agent_state = $2, end_reason = $3, end_time = $4, delete_pending = false WHERE sandbox_id = $1`,
		r.ID, int16(state), int16(reason), now); err != nil {
		return err
	}
	r.AgentState, r.EndReason, r.EndTime, r.DeletePending = state, reason, &now, false
	if err := s.writeAudit(ctx, tx, auditEvent{
		OrganizationID: r.OrgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_DELETED, Actor: actor, EndReason: reason,
		TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_SANDBOX, TargetID: r.ID,
	}); err != nil {
		return err
	}
	return events.Write(ctx, tx, events.Event{Kind: events.KindSandboxDeleted, OrganizationID: r.OrgID, SandboxID: r.ID})
}

// deleteActor returns the actor of a sandbox end: the caller of a pending
// delete, or SYSTEM.
func (r *sandboxRow) deleteActor() auth.Principal {
	if r.DeletePending && r.DeleteActor.Kind != 0 {
		return r.DeleteActor
	}
	return auth.SystemPrincipal
}

// sandboxWatch wakes RPCs that wait for a sandbox change.
type sandboxWatch struct {
	mu sync.Mutex
	m  map[string][]chan struct{}
}

// watch registers for the next change of the sandbox. The channel is closed
// by fire. The caller must call unregister when it stops waiting, whatever the
// reason; unregister is safe to call after fire and more than once.
func (w *sandboxWatch) watch(id string) (changed <-chan struct{}, unregister func()) {
	ch := make(chan struct{})
	w.mu.Lock()
	if w.m == nil {
		w.m = map[string][]chan struct{}{}
	}
	w.m[id] = append(w.m[id], ch)
	w.mu.Unlock()
	return ch, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		chans := w.m[id]
		for i, c := range chans {
			if c != ch {
				continue
			}
			// Copy so a concurrent fire that took the old slice is not affected.
			rest := make([]chan struct{}, 0, len(chans)-1)
			rest = append(rest, chans[:i]...)
			rest = append(rest, chans[i+1:]...)
			if len(rest) == 0 {
				delete(w.m, id)
			} else {
				w.m[id] = rest
			}
			return
		}
	}
}

// count reports the number of live registrations, for tests.
func (w *sandboxWatch) count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, chans := range w.m {
		n += len(chans)
	}
	return n
}

func (w *sandboxWatch) fire(id string) {
	w.mu.Lock()
	chans := w.m[id]
	delete(w.m, id)
	w.mu.Unlock()
	for _, ch := range chans {
		close(ch)
	}
}
