// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/scheduler"
)

// Sandbox is a sandbox row with its displayed state.
type Sandbox struct {
	ID              string
	OrganizationID  string
	DeviceID        string
	AgentState      ownsandv1.SandboxState
	State           ownsandv1.SandboxState // displayed
	LastKnownState  ownsandv1.SandboxState // set when State is UNREACHABLE
	EndReason       ownsandv1.SandboxEndReason
	BaseImage       ownsandv1.BaseImage
	SourceSnapshot  string
	SourceTemplate  string
	GuestOS         ownsandv1.GuestOs
	Arch            ownsandv1.Arch
	Resources       Limits
	NetworkMode     ownsandv1.NetworkMode
	IdleTTL         time.Duration
	MaxTTL          time.Duration
	IdleDeadline    time.Time
	MaxDeadline     time.Time
	Labels          map[string]string
	CreatedBy       Actor
	CreatedAt       time.Time
	EndedAt         *time.Time
	RestorePath     ownsandv1.RestorePath
	PendingDelete   bool
	DeleteReason    ownsandv1.SandboxEndReason
	RevisionEpoch   string
	RevisionCounter uint64
	MachineID       []byte
}

// Terminal reports whether a state is DELETED or FAILED.
func Terminal(s ownsandv1.SandboxState) bool {
	return s == ownsandv1.SandboxState_SANDBOX_STATE_DELETED || s == ownsandv1.SandboxState_SANDBOX_STATE_FAILED
}

// DisplayState derives the reported state: terminal states stay; a
// non-terminal sandbox of a device that is not online is UNREACHABLE with
// its last known state; a pending delete shows as DELETING.
func DisplayState(agent ownsandv1.SandboxState, pendingDelete, deviceOnline bool) (state, lastKnown ownsandv1.SandboxState) {
	if Terminal(agent) {
		return agent, ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
	}
	known := agent
	if pendingDelete {
		known = ownsandv1.SandboxState_SANDBOX_STATE_DELETING
	}
	if !deviceOnline {
		return ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE, known
	}
	return known, ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
}

// displayStateSQL mirrors DisplayState for filtering.
const displayStateSQL = `CASE WHEN s.agent_state IN (5, 6) THEN s.agent_state
	WHEN d.state IS DISTINCT FROM 1 THEN 7
	WHEN s.pending_delete THEN 4
	ELSE s.agent_state END`

const sandboxColumns = `s.sandbox_id, s.organization_id, s.device_id, s.agent_state, s.end_reason, s.base_image,
	coalesce(s.source_snapshot_id::text, ''), coalesce(s.source_template_id::text, ''), s.guest_os, s.arch,
	s.vcpu_count, s.memory_mib, s.disk_gib, s.network_mode, s.idle_ttl_ms, s.max_ttl_ms, s.idle_deadline, s.max_deadline,
	s.labels, s.created_by_kind, s.created_by_id, s.created_at, s.ended_at, s.restore_path, s.pending_delete, s.delete_reason,
	s.revision_epoch, s.revision_counter, s.macos_machine_identifier, coalesce(d.state = 1, false)`

const sandboxFrom = `sandboxes s LEFT JOIN devices d ON d.device_id = s.device_id`

func scanSandbox(r pgx.Row) (Sandbox, error) {
	var sb Sandbox
	var agent, end, base, guest, arch, net, kind, restore, delReason int16
	var idleMS, maxMS int64
	var labels []byte
	var counter int64
	var online bool
	err := r.Scan(&sb.ID, &sb.OrganizationID, &sb.DeviceID, &agent, &end, &base, &sb.SourceSnapshot, &sb.SourceTemplate, &guest, &arch,
		&sb.Resources.VCPUCount, &sb.Resources.MemoryMiB, &sb.Resources.DiskGiB, &net, &idleMS, &maxMS, &sb.IdleDeadline, &sb.MaxDeadline,
		&labels, &kind, &sb.CreatedBy.ID, &sb.CreatedAt, &sb.EndedAt, &restore, &sb.PendingDelete, &delReason,
		&sb.RevisionEpoch, &counter, &sb.MachineID, &online)
	if err != nil {
		return sb, err
	}
	sb.AgentState = ownsandv1.SandboxState(agent)
	sb.EndReason = ownsandv1.SandboxEndReason(end)
	sb.BaseImage = ownsandv1.BaseImage(base)
	sb.GuestOS = ownsandv1.GuestOs(guest)
	sb.Arch = ownsandv1.Arch(arch)
	sb.NetworkMode = ownsandv1.NetworkMode(net)
	sb.IdleTTL = time.Duration(idleMS) * time.Millisecond
	sb.MaxTTL = time.Duration(maxMS) * time.Millisecond
	sb.CreatedBy.Kind = ownsandv1.PrincipalKind(kind)
	sb.RestorePath = ownsandv1.RestorePath(restore)
	sb.DeleteReason = ownsandv1.SandboxEndReason(delReason)
	sb.RevisionCounter = uint64(counter)
	sb.State, sb.LastKnownState = DisplayState(sb.AgentState, sb.PendingDelete, online)
	if len(labels) > 0 {
		if err := json.Unmarshal(labels, &sb.Labels); err != nil {
			return sb, err
		}
	}
	return sb, nil
}

// GetSandbox returns a sandbox of the organization.
func (s *Store) GetSandbox(ctx context.Context, organizationID, sandboxID string) (Sandbox, error) {
	if !ValidID(sandboxID) || !ValidID(organizationID) {
		return Sandbox{}, ErrNotFound
	}
	sb, err := scanSandbox(s.Pool.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM `+sandboxFrom+` WHERE s.organization_id = $1 AND s.sandbox_id = $2`, organizationID, sandboxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

// GetSandboxForDevice returns a sandbox that runs on the device.
func (s *Store) GetSandboxForDevice(ctx context.Context, deviceID, sandboxID string) (Sandbox, error) {
	if !ValidID(sandboxID) {
		return Sandbox{}, ErrNotFound
	}
	sb, err := scanSandbox(s.Pool.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM `+sandboxFrom+` WHERE s.device_id = $1 AND s.sandbox_id = $2`, deviceID, sandboxID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	return sb, err
}

// ListSandboxes lists sandboxes. Ended sandboxes are listed only when states
// asks for DELETED or FAILED; states filters on the displayed state.
func (s *Store) ListSandboxes(ctx context.Context, organizationID, deviceID string, states []ownsandv1.SandboxState, p Page) ([]Sandbox, error) {
	var stateArgs []int16
	for _, st := range states {
		stateArgs = append(stateArgs, int16(st))
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+sandboxColumns+` FROM `+sandboxFrom+`
		WHERE s.organization_id = $1 AND s.sandbox_id > $2 AND ($4 = '' OR s.device_id::text = $4)
		AND (cardinality($5::smallint[]) = 0 AND s.agent_state NOT IN (5, 6) OR (`+displayStateSQL+`) = ANY($5))
		ORDER BY s.sandbox_id LIMIT $3`, organizationID, afterArg(p), p.Size, deviceID, stateArgs)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Sandbox, error) { return scanSandbox(r) })
}

// CreateSandboxParams is a validated create request.
type CreateSandboxParams struct {
	OrganizationID  string
	IdempotencyKey  string
	IdempotencyHash []byte
	BaseImage       ownsandv1.BaseImage
	SnapshotID      string
	TemplateID      string
	Resources       Limits
	NetworkMode     ownsandv1.NetworkMode
	IdleTTL         time.Duration
	MaxTTL          time.Duration
	HostOS          *ownsandv1.HostOs
	Arch            *ownsandv1.Arch
	DeviceID        string
	Labels          map[string]string
	Actor           Actor
	RequestID       string
}

// CreateSandboxResult is the reserved sandbox and how to create it.
type CreateSandboxResult struct {
	Sandbox Sandbox
	// Existing is set when the idempotency key returned the original sandbox.
	Existing      bool
	MemoryRestore bool
	Limits        Limits
	// Import is set when the device must import the exported snapshot first.
	Import *Snapshot
	// SnapshotID is the source snapshot, also for a template source.
	SnapshotID string
}

// Errors of CreateSandbox.
var (
	ErrIdempotencyMismatch = errors.New("idempotency key reused with different parameters")
	ErrSnapshotTooLarge    = errors.New("disk_gib is smaller than the snapshot disk")
	ErrSourceNotReady      = errors.New("source snapshot is not ready")
)

// LookupIdempotency returns the sandbox of a known idempotency key. It fails
// with ErrIdempotencyMismatch for another parameter hash and ErrNotFound for
// an unknown or expired key.
func (s *Store) LookupIdempotency(ctx context.Context, organizationID, key string, hash []byte) (Sandbox, error) {
	if !ValidID(key) {
		return Sandbox{}, ErrNotFound
	}
	var id string
	var stored []byte
	err := s.Pool.QueryRow(ctx, `SELECT sandbox_id, idempotency_hash FROM sandboxes WHERE organization_id = $1 AND idempotency_key = $2 AND idempotency_expires_at > $3`,
		organizationID, key, s.now()).Scan(&id, &stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return Sandbox{}, ErrNotFound
	}
	if err != nil {
		return Sandbox{}, err
	}
	if !bytes.Equal(stored, hash) {
		return Sandbox{}, ErrIdempotencyMismatch
	}
	return s.GetSandbox(ctx, organizationID, id)
}

// CreateSandbox places and reserves a sandbox in one transaction: it locks
// the candidate devices, computes their facts, runs the scheduler and inserts
// the sandbox in CREATING with the SANDBOX_CREATED audit event. live reports
// whether a device has a current agent channel on this instance.
func (s *Store) CreateSandbox(ctx context.Context, p CreateSandboxParams, live func(deviceID string) bool) (CreateSandboxResult, error) {
	var res CreateSandboxResult
	now := s.now()
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var snap *Snapshot
		guest := guestOfBaseImage(p.BaseImage)
		if p.TemplateID != "" {
			err := tx.QueryRow(ctx, `SELECT snapshot_id FROM templates WHERE organization_id = $1 AND template_id = $2`, p.OrganizationID, p.TemplateID).Scan(&p.SnapshotID)
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
		}
		if p.SnapshotID != "" {
			sn, err := scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2 FOR SHARE`, p.OrganizationID, p.SnapshotID))
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			if err != nil {
				return err
			}
			if sn.State != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
				return ErrSourceNotReady
			}
			if p.Resources.DiskGiB < sn.Resources.DiskGiB {
				return ErrSnapshotTooLarge
			}
			snap = &sn
			guest = sn.GuestOS
		}
		// Lock the candidate devices in ID order so that concurrent creates
		// serialize on the devices they compete for.
		rows, err := tx.Query(ctx, `SELECT device_id FROM devices WHERE organization_id = $1 AND state <> $2 AND ($3 = '' OR device_id::text = $3)
			ORDER BY device_id FOR UPDATE`, p.OrganizationID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED), p.DeviceID)
		if err != nil {
			return err
		}
		ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		if p.DeviceID != "" && len(ids) == 0 {
			return ErrNotFound
		}
		var revoked []string
		rows, err = tx.Query(ctx, `SELECT DISTINCT version FROM agent_releases WHERE revoked`)
		if err != nil {
			return err
		}
		if revoked, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
			return err
		}
		reserved := false
		if snap != nil && len(snap.MachineID) > 0 {
			err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sandboxes s LEFT JOIN devices d ON d.device_id = s.device_id
				WHERE s.macos_machine_identifier = $1 AND s.max_deadline > $2 AND (s.agent_state = ANY($3)
				OR (s.end_reason = $4 AND d.removal_confirmed_at IS NULL)))`,
				snap.MachineID, now, activeStates, int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED)).Scan(&reserved)
			if err != nil {
				return err
			}
		}
		var candidates []scheduler.Device
		facts := map[string]Device{}
		for _, id := range ids {
			d, err := scanDevice(tx.QueryRow(ctx, `SELECT `+deviceColumns+` FROM `+deviceFrom+` WHERE d.device_id = $2`, activeStates, id))
			if err != nil {
				return err
			}
			facts[id] = d
			var unreported, macos int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(disk_gib) FILTER (WHERE agent_state = $2 AND NOT agent_reported), 0),
				count(*) FILTER (WHERE guest_os = $3 AND agent_state = ANY($4))
				FROM sandboxes WHERE device_id = $1`, id, int16(ownsandv1.SandboxState_SANDBOX_STATE_CREATING),
				int16(ownsandv1.GuestOs_GUEST_OS_MACOS), activeStates).Scan(&unreported, &macos); err != nil {
				return err
			}
			candidates = append(candidates, scheduler.Device{
				ID: id, State: d.State, LiveChannel: live(id), Caps: d.Capabilities,
				Limits:                    scheduler.Resources(d.Limits),
				Allocated:                 scheduler.Resources(d.Allocated),
				UnreportedCreatingDiskGiB: unreported, MacOSGuests: int(macos),
				UpdateState: d.UpdateState, PendingIdleSince: d.PendingIdleSince,
				VersionRevoked:            slices.Contains(revoked, d.AgentVersion),
				MachineIdentifierReserved: reserved,
			})
		}
		req := scheduler.Request{GuestOS: guest, BaseImage: p.BaseImage, Resources: scheduler.Resources(p.Resources), NetworkMode: p.NetworkMode, HostOS: p.HostOS, Arch: p.Arch, DeviceID: p.DeviceID}
		if snap != nil {
			req.Snapshot = &scheduler.Snapshot{
				ID: snap.ID, DeviceID: snap.DeviceID, Exported: snap.ExportState == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED,
				HostOS: snap.HostOS, Arch: snap.Arch, GuestOS: snap.GuestOS, MacOSHardwareModel: snap.MacOSHardwareModel,
				MacOSMachineIdentifier: snap.MachineID, HasMemoryState: snap.HasMemoryState,
				Resources: scheduler.Resources(snap.Resources), NetworkMode: snap.NetworkMode,
			}
		}
		placement, err := scheduler.Place(candidates, req, now)
		if err != nil {
			return err
		}
		dev := facts[placement.DeviceID]
		res.SnapshotID = p.SnapshotID
		res.MemoryRestore = placement.MemoryRestore
		res.Limits = dev.Limits
		if snap != nil && snap.DeviceID != dev.ID {
			res.Import = snap
		}
		labels, err := json.Marshal(p.Labels)
		if err != nil {
			return err
		}
		sourceKind := int16(1)
		var machineID []byte
		if snap != nil {
			sourceKind = 2
			machineID = snap.MachineID
		}
		id := NewID()
		if _, err := tx.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, idempotency_key, idempotency_hash, idempotency_expires_at,
			source_kind, base_image, source_snapshot_id, source_template_id, guest_os, arch, vcpu_count, memory_mib, disk_gib, network_mode,
			idle_ttl_ms, max_ttl_ms, idle_deadline, max_deadline, labels, created_by_kind, created_by_id, created_at, agent_state, macos_machine_identifier)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)`,
			id, p.OrganizationID, dev.ID, nullString(p.IdempotencyKey), p.IdempotencyHash, now.Add(IdempotencyRetention),
			sourceKind, int16(p.BaseImage), nullString(p.SnapshotID), nullString(p.TemplateID), int16(guest), int16(dev.Capabilities.GetArch()),
			p.Resources.VCPUCount, p.Resources.MemoryMiB, p.Resources.DiskGiB, int16(p.NetworkMode),
			p.IdleTTL.Milliseconds(), p.MaxTTL.Milliseconds(), now.Add(p.IdleTTL), now.Add(p.MaxTTL), labels,
			int16(p.Actor.Kind), p.Actor.ID, now, int16(ownsandv1.SandboxState_SANDBOX_STATE_CREATING), machineID); err != nil {
			if isUniqueViolation(err) {
				return ErrConflict
			}
			return err
		}
		if err := s.audit(ctx, tx, AuditEvent{
			OrganizationID: p.OrganizationID, Action: ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_CREATED, Actor: p.Actor,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_SANDBOX, TargetID: id, RequestID: p.RequestID,
		}); err != nil {
			return err
		}
		res.Sandbox, err = scanSandbox(tx.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM `+sandboxFrom+` WHERE s.sandbox_id = $1`, id))
		return err
	})
	return res, err
}

func guestOfBaseImage(b ownsandv1.BaseImage) ownsandv1.GuestOs {
	switch b {
	case ownsandv1.BaseImage_BASE_IMAGE_LINUX:
		return ownsandv1.GuestOs_GUEST_OS_LINUX
	case ownsandv1.BaseImage_BASE_IMAGE_MACOS:
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	case ownsandv1.BaseImage_BASE_IMAGE_WINDOWS:
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS
	default:
		return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
	}
}

// RequestDelete records a pending delete and the delete control event. A
// terminal sandbox does not change. It returns the sandbox afterwards.
func (s *Store) RequestDelete(ctx context.Context, organizationID, sandboxID string, actor Actor, requestID string) (Sandbox, error) {
	if !ValidID(sandboxID) {
		return Sandbox{}, ErrNotFound
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var agent int16
		var pending bool
		var deviceID string
		err := tx.QueryRow(ctx, `SELECT agent_state, pending_delete, device_id FROM sandboxes WHERE organization_id = $1 AND sandbox_id = $2 FOR UPDATE`,
			organizationID, sandboxID).Scan(&agent, &pending, &deviceID)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil || Terminal(ownsandv1.SandboxState(agent)) || pending {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE sandboxes SET pending_delete = true, delete_reason = $2, delete_actor_kind = $3, delete_actor_id = $4 WHERE sandbox_id = $1`,
			sandboxID, int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER), int16(actor.Kind), actor.ID); err != nil {
			return err
		}
		return s.controlEvent(ctx, tx, ControlEvent{Kind: ControlEventSandboxDeleteRequested, OrganizationID: organizationID, SandboxID: sandboxID, DeviceID: deviceID})
	})
	if err != nil {
		return Sandbox{}, err
	}
	return s.GetSandbox(ctx, organizationID, sandboxID)
}

// FailCreate ends a sandbox whose create the agent rejected before it made a
// VM, which releases its reservation.
func (s *Store) FailCreate(ctx context.Context, deviceID, sandboxID string) error {
	return s.tx(ctx, func(tx pgx.Tx) error {
		var org string
		var agent int16
		err := tx.QueryRow(ctx, `SELECT organization_id, agent_state FROM sandboxes WHERE device_id = $1 AND sandbox_id = $2 FOR UPDATE`, deviceID, sandboxID).Scan(&org, &agent)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil || ownsandv1.SandboxState(agent) != ownsandv1.SandboxState_SANDBOX_STATE_CREATING {
			return err
		}
		_, err = s.endSandboxTx(ctx, tx, org, sandboxID, ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_CREATE_FAILED, Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_SYSTEM}, "")
		return err
	})
}

// ReportOutcome tells the agent channel what to do after a sandbox report.
type ReportOutcome struct {
	// Ack is set when the report must be acknowledged (applied, confirmed or stale).
	Ack bool
	// SendDelete is set when the agent still runs a VM that must be deleted.
	SendDelete   bool
	DeleteReason ownsandv1.SandboxEndReason
	// Changed is set when the stored state changed.
	Changed bool
}

// RevisionOrder compares a reported revision with the stored one.
type RevisionOrder int

const (
	RevisionOlder RevisionOrder = iota - 1
	RevisionEqual
	RevisionNewer
)

// CompareRevision orders a reported revision against the stored one. A
// revision of the device's current epoch is newer than one of any other
// epoch; a revision of an epoch other than the current one is older.
func CompareRevision(deviceEpoch, reportedEpoch string, reportedCounter uint64, storedEpoch string, storedCounter uint64) RevisionOrder {
	if reportedEpoch != deviceEpoch {
		return RevisionOlder
	}
	if storedEpoch != deviceEpoch {
		return RevisionNewer
	}
	switch {
	case reportedCounter > storedCounter:
		return RevisionNewer
	case reportedCounter == storedCounter:
		return RevisionEqual
	default:
		return RevisionOlder
	}
}

// ApplySandboxReport applies one sandbox entry of a state report or a
// sandbox update from the device.
func (s *Store) ApplySandboxReport(ctx context.Context, deviceID string, r *ownsandv1.SandboxReport) (ReportOutcome, error) {
	var out ReportOutcome
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		out, err = s.applySandboxReportTx(ctx, tx, deviceID, r)
		return err
	})
	return out, err
}

func (s *Store) applySandboxReportTx(ctx context.Context, tx pgx.Tx, deviceID string, r *ownsandv1.SandboxReport) (ReportOutcome, error) {
	out := ReportOutcome{Ack: true}
	if !ValidID(r.GetSandboxId()) {
		return out, nil
	}
	var deviceEpoch string
	if err := tx.QueryRow(ctx, `SELECT state_epoch FROM devices WHERE device_id = $1`, deviceID).Scan(&deviceEpoch); err != nil {
		return out, err
	}
	var org, epoch string
	var agent, delReason int16
	var counter int64
	var pending bool
	err := tx.QueryRow(ctx, `SELECT organization_id, agent_state, revision_epoch, revision_counter, pending_delete, delete_reason
		FROM sandboxes WHERE device_id = $1 AND sandbox_id = $2 FOR UPDATE`, deviceID, r.GetSandboxId()).
		Scan(&org, &agent, &epoch, &counter, &pending, &delReason)
	reportedLive := !Terminal(r.GetState())
	if errors.Is(err, pgx.ErrNoRows) {
		// The server has no record (for example, after retention): a live VM
		// must go.
		out.SendDelete = reportedLive
		out.DeleteReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
		return out, nil
	}
	if err != nil {
		return out, err
	}
	stored := ownsandv1.SandboxState(agent)
	if Terminal(stored) {
		// deleted and failed are absorbing.
		out.SendDelete = reportedLive
		out.DeleteReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
		return out, nil
	}
	order := CompareRevision(deviceEpoch, r.GetRevision().GetEpoch(), r.GetRevision().GetCounter(), epoch, uint64(counter))
	if order == RevisionNewer {
		state := r.GetState()
		if !Terminal(state) {
			idle := r.GetIdleDeadline().AsTime()
			if r.GetIdleDeadline() == nil {
				idle = time.Time{}
			}
			if _, err := tx.Exec(ctx, `UPDATE sandboxes SET agent_state = $2, revision_epoch = $3, revision_counter = $4, agent_reported = true,
				idle_deadline = CASE WHEN $5::timestamptz IS NULL THEN idle_deadline ELSE $5 END,
				restore_path = CASE WHEN $6 = 0 THEN restore_path ELSE $6 END WHERE sandbox_id = $1`,
				r.GetSandboxId(), int16(state), r.GetRevision().GetEpoch(), int64(r.GetRevision().GetCounter()), nullTime(idle), int16(r.GetRestorePath())); err != nil {
				return out, err
			}
		} else {
			reason := r.GetEndReason()
			if state == ownsandv1.SandboxState_SANDBOX_STATE_DELETED && pending && (reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED || reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER) {
				reason = ownsandv1.SandboxEndReason(delReason)
			}
			if reason == ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED {
				reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
			}
			if _, err := tx.Exec(ctx, `UPDATE sandboxes SET revision_epoch = $2, revision_counter = $3, agent_reported = true WHERE sandbox_id = $1`,
				r.GetSandboxId(), r.GetRevision().GetEpoch(), int64(r.GetRevision().GetCounter())); err != nil {
				return out, err
			}
			if _, err := s.endSandboxTx(ctx, tx, org, r.GetSandboxId(), state, reason, Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_SYSTEM}, ""); err != nil {
				return out, err
			}
		}
		out.Changed = true
	}
	if pending && reportedLive {
		out.SendDelete = true
		out.DeleteReason = ownsandv1.SandboxEndReason(delReason)
	}
	return out, nil
}

// StateReportOutcome is the result of reconciling a full state report.
type StateReportOutcome struct {
	// Sandboxes to delete on the device, with the end reason to send.
	Deletes map[string]ownsandv1.SandboxEndReason
	// Ended sandboxes whose sessions must end.
	Ended []string
}

// ReconcileStateReport applies a full state report in one transaction:
// listed sandboxes follow the revision rule; unlisted non-terminal sandboxes
// end (DELETED with the delete reason when a delete was pending, FAILED with
// LOST_ON_DEVICE otherwise); unlisted snapshots lose their local copy; the
// device's built images, version and update state are stored; and the device
// becomes ONLINE.
func (s *Store) ReconcileStateReport(ctx context.Context, deviceID string, rep *ownsandv1.StateReport) (StateReportOutcome, error) {
	out := StateReportOutcome{Deletes: map[string]ownsandv1.SandboxEndReason{}}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var org string
		var state int16
		if err := tx.QueryRow(ctx, `SELECT organization_id, state FROM devices WHERE device_id = $1 FOR UPDATE`, deviceID).Scan(&org, &state); err != nil {
			return err
		}
		if ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			return ErrConflict
		}
		listed := map[string]bool{}
		for _, r := range rep.GetSandboxes() {
			listed[r.GetSandboxId()] = true
			o, err := s.applySandboxReportTx(ctx, tx, deviceID, r)
			if err != nil {
				return err
			}
			if o.SendDelete {
				out.Deletes[r.GetSandboxId()] = o.DeleteReason
			}
			if o.Changed && Terminal(r.GetState()) {
				out.Ended = append(out.Ended, r.GetSandboxId())
			}
		}
		rows, err := tx.Query(ctx, `SELECT sandbox_id, pending_delete, delete_reason FROM sandboxes WHERE device_id = $1 AND agent_state NOT IN (5, 6) FOR UPDATE`, deviceID)
		if err != nil {
			return err
		}
		type rest struct {
			id      string
			pending bool
			reason  int16
		}
		others, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (rest, error) {
			var x rest
			return x, r.Scan(&x.id, &x.pending, &x.reason)
		})
		if err != nil {
			return err
		}
		for _, x := range others {
			if listed[x.id] {
				continue
			}
			st, reason := ownsandv1.SandboxState_SANDBOX_STATE_FAILED, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_LOST_ON_DEVICE
			if x.pending {
				st, reason = ownsandv1.SandboxState_SANDBOX_STATE_DELETED, ownsandv1.SandboxEndReason(x.reason)
			}
			if _, err := s.endSandboxTx(ctx, tx, org, x.id, st, reason, Actor{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_SYSTEM}, ""); err != nil {
				return err
			}
			out.Ended = append(out.Ended, x.id)
		}
		listedSnaps := map[string]bool{}
		for _, sn := range rep.GetSnapshots() {
			listedSnaps[sn.GetSnapshotId()] = true
			if err := s.applySnapshotReportTx(ctx, tx, deviceID, sn, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_UNSPECIFIED, nil); err != nil {
				return err
			}
		}
		rows, err = tx.Query(ctx, `SELECT snapshot_id::text FROM snapshots WHERE device_id = $1`, deviceID)
		if err != nil {
			return err
		}
		snaps, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range snaps {
			if !listedSnaps[id] {
				if err := s.dropSnapshotCopyTx(ctx, tx, org, id); err != nil {
					return err
				}
			}
		}
		if err := s.updateStatusTx(ctx, tx, deviceID, rep.GetAgentVersion(), rep.GetUpdateState()); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE devices SET state = $2, last_seen_at = $3 WHERE device_id = $1`, deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE), s.now()); err != nil {
			return err
		}
		if len(rep.GetBuiltBaseImages()) > 0 {
			return s.mergeCapsTx(ctx, tx, deviceID, func(c *ownsandv1.DeviceCapabilities) { c.BuiltBaseImages = rep.GetBuiltBaseImages() })
		}
		return nil
	})
	return out, err
}

// PendingDeletes lists the non-terminal sandboxes of the device with a
// pending delete.
func (s *Store) PendingDeletes(ctx context.Context, deviceID string) (map[string]ownsandv1.SandboxEndReason, error) {
	rows, err := s.Pool.Query(ctx, `SELECT sandbox_id, delete_reason FROM sandboxes WHERE device_id = $1 AND pending_delete AND agent_state NOT IN (5, 6)`, deviceID)
	if err != nil {
		return nil, err
	}
	out := map[string]ownsandv1.SandboxEndReason{}
	defer rows.Close()
	for rows.Next() {
		var id string
		var r int16
		if err := rows.Scan(&id, &r); err != nil {
			return nil, err
		}
		out[id] = ownsandv1.SandboxEndReason(r)
	}
	return out, rows.Err()
}
