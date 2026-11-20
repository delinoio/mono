// SPDX-License-Identifier: FSL-1.1-ALv2

package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Snapshot is a snapshot row.
type Snapshot struct {
	ID                 string
	OrganizationID     string
	SandboxID          string
	DeviceID           string
	State              ownsandv1.SnapshotState
	ExportState        ownsandv1.SnapshotExportState
	GuestOS            ownsandv1.GuestOs
	Arch               ownsandv1.Arch
	HostOS             ownsandv1.HostOs
	Resources          Limits
	NetworkMode        ownsandv1.NetworkMode
	HasMemoryState     bool
	SizeBytes          int64
	MacOSHardwareModel string
	MachineID          []byte
	PartSHA256         [][]byte
	CreatedBy          Actor
	CreatedAt          time.Time
}

const snapshotColumns = `snapshot_id, organization_id, sandbox_id, coalesce(device_id::text, ''), state, export_state, guest_os, arch, host_os,
	vcpu_count, memory_mib, disk_gib, network_mode, has_memory_state, size_bytes, macos_hardware_model, macos_machine_identifier,
	part_sha256, created_by_kind, created_by_id, created_at`

func scanSnapshot(r pgx.Row) (Snapshot, error) {
	var sn Snapshot
	var state, export, guest, arch, host, net, kind int16
	err := r.Scan(&sn.ID, &sn.OrganizationID, &sn.SandboxID, &sn.DeviceID, &state, &export, &guest, &arch, &host,
		&sn.Resources.VCPUCount, &sn.Resources.MemoryMiB, &sn.Resources.DiskGiB, &net, &sn.HasMemoryState, &sn.SizeBytes,
		&sn.MacOSHardwareModel, &sn.MachineID, &sn.PartSHA256, &kind, &sn.CreatedBy.ID, &sn.CreatedAt)
	sn.State = ownsandv1.SnapshotState(state)
	sn.ExportState = ownsandv1.SnapshotExportState(export)
	sn.GuestOS = ownsandv1.GuestOs(guest)
	sn.Arch = ownsandv1.Arch(arch)
	sn.HostOS = ownsandv1.HostOs(host)
	sn.NetworkMode = ownsandv1.NetworkMode(net)
	sn.CreatedBy.Kind = ownsandv1.PrincipalKind(kind)
	return sn, err
}

// GetSnapshot returns a snapshot of the organization.
func (s *Store) GetSnapshot(ctx context.Context, organizationID, snapshotID string) (Snapshot, error) {
	if !ValidID(snapshotID) {
		return Snapshot{}, ErrNotFound
	}
	sn, err := scanSnapshot(s.Pool.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2`, organizationID, snapshotID))
	if errors.Is(err, pgx.ErrNoRows) {
		return Snapshot{}, ErrNotFound
	}
	return sn, err
}

// ListSnapshots lists the organization's snapshots.
func (s *Store) ListSnapshots(ctx context.Context, organizationID, deviceID string, p Page) ([]Snapshot, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE organization_id = $1 AND snapshot_id > $2
		AND ($4 = '' OR device_id::text = $4) ORDER BY snapshot_id LIMIT $3`, organizationID, afterArg(p), p.Size, deviceID)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Snapshot, error) { return scanSnapshot(r) })
}

// CreateSnapshotRecord stores a new snapshot of a sandbox in CREATING. The
// sandbox must be RUNNING or PAUSED (ErrConflict otherwise).
func (s *Store) CreateSnapshotRecord(ctx context.Context, organizationID, sandboxID string, actor Actor) (Snapshot, error) {
	var sn Snapshot
	err := s.tx(ctx, func(tx pgx.Tx) error {
		sb, err := scanSandbox(tx.QueryRow(ctx, `SELECT `+sandboxColumns+` FROM `+sandboxFrom+` WHERE s.organization_id = $1 AND s.sandbox_id = $2 FOR UPDATE OF s`, organizationID, sandboxID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if sb.State != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && sb.State != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			return ErrConflict
		}
		var caps []byte
		if err := tx.QueryRow(ctx, `SELECT capabilities FROM devices WHERE device_id = $1`, sb.DeviceID).Scan(&caps); err != nil {
			return err
		}
		c := &ownsandv1.DeviceCapabilities{}
		if err := proto.Unmarshal(caps, c); err != nil {
			return err
		}
		sn = Snapshot{
			ID: NewID(), OrganizationID: organizationID, SandboxID: sandboxID, DeviceID: sb.DeviceID,
			State: ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING, ExportState: ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED,
			GuestOS: sb.GuestOS, Arch: sb.Arch, HostOS: c.GetHostOs(), Resources: sb.Resources, NetworkMode: sb.NetworkMode,
			MachineID: sb.MachineID, CreatedBy: actor, CreatedAt: s.now(),
		}
		_, err = tx.Exec(ctx, `INSERT INTO snapshots (snapshot_id, organization_id, sandbox_id, device_id, state, export_state, guest_os, arch, host_os,
			vcpu_count, memory_mib, disk_gib, network_mode, macos_machine_identifier, created_by_kind, created_by_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)`,
			sn.ID, organizationID, sandboxID, sn.DeviceID, int16(sn.State), int16(sn.ExportState), int16(sn.GuestOS), int16(sn.Arch), int16(sn.HostOS),
			sn.Resources.VCPUCount, sn.Resources.MemoryMiB, sn.Resources.DiskGiB, int16(sn.NetworkMode), sn.MachineID, int16(actor.Kind), actor.ID, sn.CreatedAt)
		return err
	})
	return sn, err
}

// ApplySnapshotReport stores a snapshot report or update from its device.
func (s *Store) ApplySnapshotReport(ctx context.Context, deviceID string, r *ownsandv1.SnapshotReport, export ownsandv1.SnapshotExportState, parts [][]byte) error {
	return s.tx(ctx, func(tx pgx.Tx) error { return s.applySnapshotReportTx(ctx, tx, deviceID, r, export, parts) })
}

func (s *Store) applySnapshotReportTx(ctx context.Context, tx pgx.Tx, deviceID string, r *ownsandv1.SnapshotReport, export ownsandv1.SnapshotExportState, parts [][]byte) error {
	if !ValidID(r.GetSnapshotId()) {
		return nil
	}
	var org string
	var current int16
	err := tx.QueryRow(ctx, `SELECT organization_id, export_state FROM snapshots WHERE snapshot_id = $1 AND (device_id = $2 OR (device_id IS NULL AND export_state = $3)) FOR UPDATE`,
		r.GetSnapshotId(), deviceID, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)).Scan(&org, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if r.GetState() == ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED && ownsandv1.SnapshotExportState(current) == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED {
		_, err := tx.Exec(ctx, `UPDATE snapshots SET state = $2 WHERE snapshot_id = $1`, r.GetSnapshotId(), int16(r.GetState()))
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE snapshots SET state = CASE WHEN $2 = 0 THEN state ELSE $2 END, device_id = $6,
		size_bytes = CASE WHEN $3 = 0 THEN size_bytes ELSE $3 END, has_memory_state = $4,
		macos_hardware_model = CASE WHEN $5 = '' THEN macos_hardware_model ELSE $5 END WHERE snapshot_id = $1`,
		r.GetSnapshotId(), int16(r.GetState()), r.GetSizeBytes(), r.GetHasMemoryState(), r.GetMacosHardwareModel(), deviceID); err != nil {
		return err
	}
	if export != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_UNSPECIFIED && ownsandv1.SnapshotExportState(current) != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED {
		if export == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED {
			if _, err := tx.Exec(ctx, `UPDATE snapshots SET export_state = $2, part_sha256 = $3 WHERE snapshot_id = $1`, r.GetSnapshotId(), int16(export), parts); err != nil {
				return err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1`, r.GetSnapshotId(), int16(export)); err != nil {
			return err
		}
	}
	return nil
}

// dropSnapshotCopyTx removes the local copy of a snapshot that its device no
// longer has: an exported snapshot keeps its record without a device; any
// other record is deleted with its templates, and a started export gets a
// clean-up job.
func (s *Store) dropSnapshotCopyTx(ctx context.Context, tx pgx.Tx, organizationID, snapshotID string) error {
	var export int16
	if err := tx.QueryRow(ctx, `SELECT export_state FROM snapshots WHERE snapshot_id = $1 FOR UPDATE`, snapshotID).Scan(&export); err != nil {
		return err
	}
	switch ownsandv1.SnapshotExportState(export) {
	case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED:
		_, err := tx.Exec(ctx, `UPDATE snapshots SET device_id = NULL WHERE snapshot_id = $1`, snapshotID)
		return err
	case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED:
	default:
		if err := s.cleanupJobTx(ctx, tx, organizationID, SnapshotPrefix(organizationID, snapshotID), snapshotID); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM snapshots WHERE snapshot_id = $1`, snapshotID)
	return err
}

// ErrReferenced is returned when a template references the snapshot.
var ErrReferenced = errors.New("a template references the snapshot")

// DeleteSnapshot deletes a snapshot record that no template references. A
// started export is cancelled through an R2 clean-up job. It returns the
// device that holds the local copy, if any.
func (s *Store) DeleteSnapshot(ctx context.Context, organizationID, snapshotID string) (string, error) {
	if !ValidID(snapshotID) {
		return "", ErrNotFound
	}
	var deviceID string
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var export int16
		err := tx.QueryRow(ctx, `SELECT coalesce(device_id::text, ''), export_state FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2 FOR UPDATE`,
			organizationID, snapshotID).Scan(&deviceID, &export)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		var refs bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM templates WHERE snapshot_id = $1)`, snapshotID).Scan(&refs); err != nil {
			return err
		}
		if refs {
			return ErrReferenced
		}
		if ownsandv1.SnapshotExportState(export) != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED {
			if err := s.cleanupJobTx(ctx, tx, organizationID, SnapshotPrefix(organizationID, snapshotID), snapshotID); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM snapshots WHERE snapshot_id = $1`, snapshotID)
		return err
	})
	return deviceID, err
}

// StartExport moves a ready snapshot with a local copy to EXPORTING. An
// exported or exporting snapshot is returned unchanged (changed=false).
func (s *Store) StartExport(ctx context.Context, organizationID, snapshotID string) (Snapshot, bool, error) {
	var sn Snapshot
	changed := false
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var err error
		sn, err = scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2 FOR UPDATE`, organizationID, snapshotID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch sn.ExportState {
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING:
			return nil
		}
		if sn.State != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY || sn.DeviceID == "" {
			return ErrConflict
		}
		sn.ExportState = ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING
		changed = true
		_, err = tx.Exec(ctx, `UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1`, snapshotID, int16(sn.ExportState))
		return err
	})
	return sn, changed, err
}

// SignedURLPurpose is checked before the control plane signs part URLs.
type SignedURLPurpose int

const (
	SignedURLUpload SignedURLPurpose = iota + 1
	SignedURLDownload
)

// AuthorizeSignedURLs checks that the device may get signed URLs for the
// snapshot: an upload only for the device that runs its export (EXPORTING),
// a download only for a device of the organization with the snapshot's OS
// and architecture while the snapshot is exported. For uploads it records
// the expiry of the last upload URL.
func (s *Store) AuthorizeSignedURLs(ctx context.Context, deviceID, snapshotID string, purpose SignedURLPurpose, expiresAt time.Time) (organizationID string, err error) {
	if !ValidID(snapshotID) {
		return "", ErrNotFound
	}
	err = s.tx(ctx, func(tx pgx.Tx) error {
		var devOrg string
		var caps []byte
		var state int16
		if err := tx.QueryRow(ctx, `SELECT d.organization_id, d.capabilities, d.state FROM devices d JOIN organizations o USING (organization_id)
			WHERE d.device_id = $1 AND o.deleted_at IS NULL`, deviceID).Scan(&devOrg, &caps, &state); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return ErrNotFound
			}
			return err
		}
		if ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			return ErrNotFound
		}
		sn, err := scanSnapshot(tx.QueryRow(ctx, `SELECT `+snapshotColumns+` FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2`, devOrg, snapshotID))
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		switch purpose {
		case SignedURLUpload:
			if sn.DeviceID != deviceID || sn.ExportState != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING {
				return ErrConflict
			}
			if _, err := tx.Exec(ctx, `INSERT INTO signed_upload_issues (organization_id, snapshot_id, last_expires_at) VALUES ($1, $2, $3)
				ON CONFLICT (organization_id, snapshot_id) DO UPDATE SET last_expires_at = greatest(signed_upload_issues.last_expires_at, EXCLUDED.last_expires_at)`,
				devOrg, snapshotID, expiresAt); err != nil {
				return err
			}
		case SignedURLDownload:
			c := &ownsandv1.DeviceCapabilities{}
			if err := proto.Unmarshal(caps, c); err != nil {
				return err
			}
			if sn.ExportState != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED || c.GetHostOs() != sn.HostOS || c.GetArch() != sn.Arch {
				return ErrConflict
			}
		default:
			return ErrConflict
		}
		organizationID = devOrg
		return nil
	})
	return organizationID, err
}

// Template is a template row.
type Template struct {
	ID             string
	OrganizationID string
	Name           string
	SnapshotID     string
	CreatedBy      Actor
	CreatedAt      time.Time
}

// CreateTemplate names a ready snapshot. A duplicate name fails with
// ErrAlreadyExists; a snapshot that is not ready fails with ErrConflict.
func (s *Store) CreateTemplate(ctx context.Context, organizationID, snapshotID, name string, actor Actor) (Template, error) {
	t := Template{ID: NewID(), OrganizationID: organizationID, Name: name, SnapshotID: snapshotID, CreatedBy: actor, CreatedAt: s.now()}
	if !ValidID(snapshotID) {
		return t, ErrNotFound
	}
	err := s.tx(ctx, func(tx pgx.Tx) error {
		var state int16
		err := tx.QueryRow(ctx, `SELECT state FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2 FOR SHARE`, organizationID, snapshotID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if ownsandv1.SnapshotState(state) != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			return ErrConflict
		}
		_, err = tx.Exec(ctx, `INSERT INTO templates (template_id, organization_id, name, snapshot_id, created_by_kind, created_by_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, t.ID, organizationID, name, snapshotID, int16(actor.Kind), actor.ID, t.CreatedAt)
		if isUniqueViolation(err) {
			return ErrAlreadyExists
		}
		return err
	})
	return t, err
}

// ListTemplates lists the organization's templates.
func (s *Store) ListTemplates(ctx context.Context, organizationID string, p Page) ([]Template, error) {
	rows, err := s.Pool.Query(ctx, `SELECT template_id, organization_id, name, snapshot_id, created_by_kind, created_by_id, created_at
		FROM templates WHERE organization_id = $1 AND template_id > $2 ORDER BY template_id LIMIT $3`, organizationID, afterArg(p), p.Size)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Template, error) {
		var t Template
		var kind int16
		err := r.Scan(&t.ID, &t.OrganizationID, &t.Name, &t.SnapshotID, &kind, &t.CreatedBy.ID, &t.CreatedAt)
		t.CreatedBy.Kind = ownsandv1.PrincipalKind(kind)
		return t, err
	})
}

// DeleteTemplate deletes a template; the snapshot stays.
func (s *Store) DeleteTemplate(ctx context.Context, organizationID, templateID string) error {
	if !ValidID(templateID) {
		return ErrNotFound
	}
	tag, err := s.Pool.Exec(ctx, `DELETE FROM templates WHERE organization_id = $1 AND template_id = $2`, organizationID, templateID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetDeviceOffline marks an online device offline.
func (s *Store) SetDeviceOffline(ctx context.Context, deviceID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE devices SET state = $2 WHERE device_id = $1 AND state = $3`, deviceID,
		int16(ownsandv1.DeviceState_DEVICE_STATE_OFFLINE), int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE))
	return err
}

// OnlineDevicesWithoutHeartbeat lists online devices last seen before cutoff.
func (s *Store) OnlineDevicesWithoutHeartbeat(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.Pool.Query(ctx, `SELECT device_id::text FROM devices WHERE state = $1 AND (last_seen_at IS NULL OR last_seen_at < $2)`,
		int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE), cutoff)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TouchDevice records a heartbeat.
func (s *Store) TouchDevice(ctx context.Context, deviceID string) error {
	_, err := s.Pool.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE device_id = $1`, deviceID, s.now())
	return err
}

// UpdateCapabilities stores a capability report.
func (s *Store) UpdateCapabilities(ctx context.Context, deviceID string, caps *ownsandv1.DeviceCapabilities) error {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(caps)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `UPDATE devices SET capabilities = $2, agent_version = CASE WHEN $3 = '' THEN agent_version ELSE $3 END WHERE device_id = $1`,
		deviceID, b, caps.GetAgentVersion())
	return err
}

func (s *Store) mergeCapsTx(ctx context.Context, tx pgx.Tx, deviceID string, mut func(*ownsandv1.DeviceCapabilities)) error {
	var b []byte
	if err := tx.QueryRow(ctx, `SELECT capabilities FROM devices WHERE device_id = $1`, deviceID).Scan(&b); err != nil {
		return err
	}
	c := &ownsandv1.DeviceCapabilities{}
	if err := proto.Unmarshal(b, c); err != nil {
		return err
	}
	mut(c)
	nb, err := proto.MarshalOptions{Deterministic: true}.Marshal(c)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE devices SET capabilities = $2 WHERE device_id = $1`, deviceID, nb)
	return err
}

// UpdateStatus stores the agent version and update state. pending_idle_since
// is set at the first PENDING_IDLE report and cleared by any other state.
func (s *Store) UpdateStatus(ctx context.Context, deviceID, version string, state ownsandv1.AgentUpdateState) error {
	return s.tx(ctx, func(tx pgx.Tx) error { return s.updateStatusTx(ctx, tx, deviceID, version, state) })
}

func (s *Store) updateStatusTx(ctx context.Context, tx pgx.Tx, deviceID, version string, state ownsandv1.AgentUpdateState) error {
	if state == ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_UNSPECIFIED && version == "" {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE devices SET
		agent_version = CASE WHEN $2 = '' THEN agent_version ELSE $2 END,
		update_state = CASE WHEN $3 = 0 THEN update_state ELSE $3 END,
		pending_idle_since = CASE WHEN $3 = 0 THEN pending_idle_since WHEN $3 = $4 THEN coalesce(pending_idle_since, $5) ELSE NULL END
		WHERE device_id = $1`, deviceID, version, int16(state), int16(ownsandv1.AgentUpdateState_AGENT_UPDATE_STATE_PENDING_IDLE), s.now())
	return err
}
