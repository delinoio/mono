// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// SnapshotHandler serves SnapshotService.
type SnapshotHandler struct{ *Service }

var _ ownsandv1connect.SnapshotServiceHandler = SnapshotHandler{}

// maxSnapshotParts bounds the part numbers of one export.
const maxSnapshotParts = 10000

const snapshotColumns = `snapshot_id::text, organization_id::text, source_sandbox_id::text, COALESCE(device_id::text, ''), state, export_state,
	guest_os, host_os, arch, vcpu_count, memory_mib, disk_gib, network_mode, has_memory_state, size_bytes, COALESCE(macos_hardware_model, ''::bytea),
	COALESCE(machine_lineage::text, ''), created_by_kind, created_by_id, create_time,
	COALESCE(export_command_id::text, ''), COALESCE(export_channel_id::text, '')`

type snapshotRow struct {
	pb      *ownsandv1.Snapshot
	lineage string
	// exportCommandID is the command ID of the latest export attempt; it is
	// also the attempt ID in the object keys of the attempt's parts. It is
	// empty for a snapshot that was never exported.
	exportCommandID string
	// exportChannelID is the ID of the agent channel that received the
	// command of the latest export attempt.
	exportChannelID string
}

func scanSnapshot(row pgx.Row) (*snapshotRow, error) {
	var sn ownsandv1.Snapshot
	var state, export, guest, host, arch, network, kind int16
	var createdBy, lineage, exportCommand, exportChannel string
	var created time.Time
	var res ownsandv1.Resources
	if err := row.Scan(&sn.SnapshotId, &sn.OrganizationId, &sn.SourceSandboxId, &sn.DeviceId, &state, &export, &guest, &host, &arch,
		&res.VcpuCount, &res.MemoryMib, &res.DiskGib, &network, &sn.HasMemoryState, &sn.SizeBytes, &sn.MacosHardwareModel, &lineage,
		&kind, &createdBy, &created, &exportCommand, &exportChannel); err != nil {
		return nil, err
	}
	sn.State = ownsandv1.SnapshotState(state)
	sn.ExportState = ownsandv1.SnapshotExportState(export)
	sn.GuestOs = ownsandv1.GuestOs(guest)
	sn.HostOs = ownsandv1.HostOs(host)
	sn.Arch = ownsandv1.Arch(arch)
	sn.Resources = &res
	sn.NetworkMode = ownsandv1.NetworkMode(network)
	sn.CreatedBy = principalProto(kind, createdBy)
	sn.CreateTime = ts(created)
	if len(sn.MacosHardwareModel) == 0 {
		sn.MacosHardwareModel = nil
	}
	return &snapshotRow{pb: &sn, lineage: lineage, exportCommandID: exportCommand, exportChannelID: exportChannel}, nil
}

func loadSnapshot(ctx context.Context, q db.Querier, orgID, snapshotID string, lock bool) (*snapshotRow, error) {
	if lock {
		return loadSnapshotLocked(ctx, q, orgID, snapshotID, " FOR UPDATE")
	}
	return loadSnapshotLocked(ctx, q, orgID, snapshotID, "")
}

// loadSnapshotShared loads a snapshot row with FOR SHARE: the row cannot be
// deleted or updated until the transaction ends, while other FOR SHARE
// readers proceed.
func loadSnapshotShared(ctx context.Context, q db.Querier, orgID, snapshotID string) (*snapshotRow, error) {
	return loadSnapshotLocked(ctx, q, orgID, snapshotID, " FOR SHARE")
}

func loadSnapshotLocked(ctx context.Context, q db.Querier, orgID, snapshotID, lockClause string) (*snapshotRow, error) {
	sql := "SELECT " + snapshotColumns + " FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2" + lockClause
	sn, err := scanSnapshot(q.QueryRow(ctx, sql, orgID, snapshotID))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("snapshot")
	}
	return sn, err
}

// stateErr applies the sandbox state rule of a table row: unavailable for
// UNREACHABLE and failed_precondition for the other states.
func stateErr(state ownsandv1.SandboxState, rpc string) error {
	if state == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE {
		return errUnavailable("the sandbox's device is offline")
	}
	return errPrecondition(fmt.Sprintf("%s is not allowed in state %s", rpc, state))
}

func (h SnapshotHandler) CreateSnapshot(ctx context.Context, req *connect.Request[ownsandv1.CreateSnapshotRequest]) (*connect.Response[ownsandv1.CreateSnapshotResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	p, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	if err := validateID("sandbox_id", req.Msg.GetSandboxId()); err != nil {
		return nil, err
	}
	var snap *snapshotRow
	var conn *hub.Conn
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		// Lock the organization row before the sandbox row, as
		// DeleteOrganization does: the snapshot insert takes FOR KEY SHARE
		// on it through its foreign key.
		if err := lockOrganizationShare(ctx, tx, orgID); err != nil {
			return err
		}
		sb, err := loadSandbox(ctx, tx, orgID, req.Msg.GetSandboxId(), true)
		if err != nil {
			return err
		}
		state, _ := sb.shownState()
		if state != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING && state != ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			return stateErr(state, "CreateSnapshot")
		}
		conn = h.Hub.Live(sb.DeviceID)
		if conn == nil {
			return errUnavailable("the sandbox's device has no live agent channel")
		}
		id := newID()
		lineage := any(nil)
		if sb.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
			lineage = sb.ID
			if sb.MachineLineage != "" {
				lineage = sb.MachineLineage
			}
		}
		snap, err = scanSnapshot(tx.QueryRow(ctx, `INSERT INTO snapshots (snapshot_id, organization_id, source_sandbox_id, device_id, state, export_state,
			guest_os, host_os, arch, vcpu_count, memory_mib, disk_gib, network_mode, machine_lineage, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17) RETURNING `+snapshotColumns,
			id, orgID, sb.ID, sb.DeviceID, int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING), int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED),
			int16(sb.GuestOS), int16(sb.HostOS), int16(sb.Arch), sb.VCPUCount, sb.MemoryMiB, sb.DiskGiB, int16(sb.NetworkMode), lineage, int16(p.Kind), p.ID, h.now()))
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "create snapshot", err)
	}
	if _, err := h.sendCommand(ctx, conn, commandInfo{kind: cmdCreateSnapshot, orgID: orgID, sandboxID: snap.pb.GetSourceSandboxId(), snapshotID: snap.pb.GetSnapshotId()},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSnapshot{CreateSnapshot: &ownsandv1.CreateSnapshotCommand{
			SnapshotId: snap.pb.GetSnapshotId(), SandboxId: snap.pb.GetSourceSandboxId(),
		}}}); err != nil {
		_, _ = h.DB.Pool.Exec(ctx, "UPDATE snapshots SET state = $2 WHERE snapshot_id = $1", snap.pb.GetSnapshotId(), int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED))
		return nil, errUnavailable("the agent channel closed")
	}
	return connect.NewResponse(&ownsandv1.CreateSnapshotResponse{Snapshot: snap.pb}), nil
}

func (s *Service) applySnapshotResult(ctx context.Context, info commandInfo, res *ownsandv1.AgentCommandResult) error {
	if res.GetError() != nil || res.GetCreateSnapshot().GetSnapshot() == nil {
		_, err := s.DB.Pool.Exec(ctx, "UPDATE snapshots SET state = $2 WHERE snapshot_id = $1 AND state = $3", info.snapshotID,
			int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED), int16(ownsandv1.SnapshotState_SNAPSHOT_STATE_CREATING))
		return err
	}
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		return applySnapshotEntryTx(ctx, tx, info.snapshotID, res.GetCreateSnapshot().GetSnapshot())
	})
}

// applyExportResult applies the result of an export command. Only the result
// of the attempt that the snapshot row names (its export command ID) in the
// EXPORTING state changes the row: a result of an earlier attempt that
// arrives late is ignored and leaves the state and the part manifest of the
// current attempt as they are.
func (s *Service) applyExportResult(ctx context.Context, info commandInfo, commandID string, res *ownsandv1.AgentCommandResult) error {
	return s.DB.InTx(ctx, func(tx pgx.Tx) error {
		var export int16
		var owner string
		err := tx.QueryRow(ctx, "SELECT export_state, COALESCE(export_command_id::text, '') FROM snapshots WHERE snapshot_id = $1 FOR UPDATE", info.snapshotID).Scan(&export, &owner)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // deleted meanwhile; its clean-up job removes the parts
		}
		if err != nil {
			return err
		}
		if ownsandv1.SnapshotExportState(export) != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING || owner != commandID {
			s.Logger.InfoContext(ctx, "export_result_stale", slog.String("snapshot_id", info.snapshotID), slog.String("command_id", commandID),
				slog.String("current_command_id", owner), slog.String("export_state", ownsandv1.SnapshotExportState(export).String()))
			return nil
		}
		result := res.GetExportSnapshot()
		reason := ""
		switch {
		case res.GetError() != nil:
			reason = "agent_error_" + res.GetError().GetCode().String()
		case result == nil || len(result.GetParts()) == 0:
			reason = "no_parts"
		default:
			for _, part := range result.GetParts() {
				if part.GetPartNumber() < 1 || part.GetPartNumber() > maxSnapshotParts || len(part.GetSha256()) != 32 || part.GetSizeBytes() < 0 {
					reason = "invalid_part"
					break
				}
			}
		}
		if reason != "" {
			return s.failExportTx(ctx, tx, info.orgID, info.snapshotID, commandID, reason)
		}
		if _, err := tx.Exec(ctx, "DELETE FROM snapshot_parts WHERE snapshot_id = $1", info.snapshotID); err != nil {
			return err
		}
		for _, part := range result.GetParts() {
			if _, err := tx.Exec(ctx, "INSERT INTO snapshot_parts (snapshot_id, part_number, size_bytes, sha256) VALUES ($1, $2, $3, $4)",
				info.snapshotID, part.GetPartNumber(), part.GetSizeBytes(), part.GetSha256()); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, "UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1", info.snapshotID, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)); err != nil {
			return err
		}
		s.Logger.InfoContext(ctx, "export_completed", slog.String("snapshot_id", info.snapshotID), slog.String("command_id", commandID), slog.Int("parts", len(result.GetParts())))
		return nil
	})
}

// failExportTx sets the EXPORTING snapshot snapshotID to EXPORT_FAILED and
// records a clean-up job for the objects that the attempt attemptID may have
// uploaded. The caller holds
// the snapshot row with FOR UPDATE, so the job's final_sweep_after is
// computed after the last upload URL of the attempt was signed: the attempt
// can sign no more URL once the row is not EXPORTING. The failed attempt's
// objects never outlive the sweep, and a retry writes to the prefix of its
// own attempt.
func (s *Service) failExportTx(ctx context.Context, tx pgx.Tx, orgID, snapshotID, attemptID, reason string) error {
	if _, err := tx.Exec(ctx, "UPDATE snapshots SET export_state = $2 WHERE snapshot_id = $1", snapshotID, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED)); err != nil {
		return err
	}
	if err := s.startCleanupJobTx(ctx, tx, orgID, attemptPrefix(orgID, snapshotID, attemptID)); err != nil {
		return err
	}
	s.Logger.WarnContext(ctx, "export_failed", slog.String("snapshot_id", snapshotID), slog.String("command_id", attemptID), slog.String("reason", reason))
	return nil
}

// attemptPrefix is the R2 key prefix of the objects of one export attempt.
func attemptPrefix(orgID, snapshotID, attemptID string) string {
	return snapshotPrefix(orgID, snapshotID) + "attempts/" + attemptID + "/"
}

// partKey is the R2 object key of one exported part of the attempt
// attemptID.
func partKey(orgID, snapshotID, attemptID string, part int32) string {
	return fmt.Sprintf("%s%d", attemptPrefix(orgID, snapshotID, attemptID), part)
}

// handleSignedURLRequest issues fresh part URLs to the agent that runs an
// export (UPLOAD, the channel that owns the export attempt of the snapshot
// while it is EXPORTING; the URLs address the objects of that attempt only)
// or an import (DOWNLOAD, the channel to which the server assigned the
// import command of the snapshot, on a device of the organization with the
// same OS and architecture while the snapshot is EXPORTED, for as long as
// importThenCreate waits for the import). URLs expire after 15 minutes and
// are never logged; a denied request is logged as signed_url_denied.
//
// The eligibility check and the signing run in one transaction that holds
// the snapshot row with FOR SHARE, so snapshot and organization deletion
// (whose row DELETE conflicts with that lock) wait for the signer and
// record their clean-up job afterwards: every URL expires at or before the
// job's final_sweep_after, and a request that loses the race finds no row
// and gets no URL. The transaction ends before the grant is written to the
// agent channel, so a slow channel never holds the lock. The lock order is
// the snapshot row only: the signer takes no device or organization row
// lock.
func (s *Service) handleSignedURLRequest(ctx context.Context, conn *hub.Conn, r *ownsandv1.SignedUrlRequest) error {
	grant := &ownsandv1.SignedUrlGrant{RequestId: r.GetRequestId(), SnapshotId: r.GetSnapshotId(), Operation: r.GetOperation()}
	send := func() error {
		return conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlGrant{SignedUrlGrant: grant}})
	}
	deny := func(code ownsandv1.ErrorCode, msg string) error {
		grant.Error = &ownsandv1.ErrorDetail{Code: code, Message: msg}
		grant.PartUrls = nil
		return send()
	}
	if s.Objects == nil {
		return deny(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "snapshot export storage is not configured")
	}
	if !validID(r.GetSnapshotId()) || len(r.GetPartNumbers()) == 0 || len(r.GetPartNumbers()) > maxSnapshotParts {
		return deny(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid signed URL request")
	}
	// denied carries a refusal out of the transaction, which ends before the
	// grant is sent.
	var denied *ownsandv1.ErrorDetail
	refuse := func(code ownsandv1.ErrorCode, msg string) error {
		denied = &ownsandv1.ErrorDetail{Code: code, Message: msg}
		return nil
	}
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		sn, err := loadSnapshotShared(ctx, tx, conn.OrganizationID, r.GetSnapshotId())
		if err != nil {
			if connect.CodeOf(err) == connect.CodeNotFound {
				return refuse(ownsandv1.ErrorCode_ERROR_CODE_NOT_FOUND, "snapshot not found")
			}
			return err
		}
		var host, arch int16
		if err := tx.QueryRow(ctx, "SELECT host_os, arch FROM devices WHERE device_id = $1", conn.DeviceID).Scan(&host, &arch); err != nil {
			return err
		}
		var presign func(string, time.Duration) (string, time.Time, error)
		switch r.GetOperation() {
		case ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_UPLOAD:
			// The owner is the channel that received the command of the
			// attempt: a channel that replaced it gets no URL.
			if sn.pb.GetExportState() != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING || sn.pb.GetDeviceId() != conn.DeviceID ||
				sn.exportChannelID != conn.ID {
				return refuse(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "the snapshot is not being exported by this channel")
			}
			presign = s.Objects.PresignPut
		case ownsandv1.SignedUrlOperation_SIGNED_URL_OPERATION_DOWNLOAD:
			if sn.pb.GetExportState() != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED ||
				ownsandv1.HostOs(host) != sn.pb.GetHostOs() || ownsandv1.Arch(arch) != sn.pb.GetArch() {
				return refuse(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "the snapshot is not exported for this device")
			}
			// Compatibility is not an assignment: only the channel that
			// runs the import the server sent gets URLs, so the signer is
			// not called for any other request.
			if !s.imports.assigned(conn, sn.pb.GetSnapshotId()) {
				return refuse(ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, "no import of the snapshot is assigned to this channel")
			}
			presign = s.Objects.PresignGet
		default:
			return refuse(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid operation")
		}
		var urls []*ownsandv1.SignedPartUrl
		for _, n := range r.GetPartNumbers() {
			if n < 1 || n > maxSnapshotParts {
				return refuse(ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "invalid part number")
			}
			// Uploads use the current attempt; downloads use the successful
			// attempt, which the row keeps.
			u, exp, err := presign(partKey(conn.OrganizationID, sn.pb.GetSnapshotId(), sn.exportCommandID, n), signedURLLifetime)
			if err != nil {
				return refuse(ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, "signing failed")
			}
			urls = append(urls, &ownsandv1.SignedPartUrl{PartNumber: n, Url: u, ExpireTime: ts(exp)})
		}
		grant.PartUrls = urls
		return nil
	})
	if err != nil {
		return err
	}
	if denied != nil {
		// The reason is a fixed message; no URL or credential is logged.
		s.Logger.WarnContext(ctx, "signed_url_denied", slog.String("device_id", conn.DeviceID), slog.String("snapshot_id", r.GetSnapshotId()),
			slog.String("channel_id", conn.ID), slog.String("operation", r.GetOperation().String()), slog.String("reason", denied.GetMessage()))
		return deny(denied.GetCode(), denied.GetMessage())
	}
	s.Logger.InfoContext(ctx, "signed_urls_issued", slog.String("device_id", conn.DeviceID), slog.String("snapshot_id", grant.GetSnapshotId()),
		slog.String("operation", r.GetOperation().String()), slog.Int("parts", len(grant.PartUrls)))
	return send()
}

func validID(id string) bool { return validateID("id", id) == nil }

func (h SnapshotHandler) GetSnapshot(ctx context.Context, req *connect.Request[ownsandv1.GetSnapshotRequest]) (*connect.Response[ownsandv1.GetSnapshotResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	if err := validateID("snapshot_id", req.Msg.GetSnapshotId()); err != nil {
		return nil, err
	}
	sn, err := loadSnapshot(ctx, h.DB.Pool, orgID, req.Msg.GetSnapshotId(), false)
	if err != nil {
		return nil, h.internal(ctx, "get snapshot", err)
	}
	return connect.NewResponse(&ownsandv1.GetSnapshotResponse{Snapshot: sn.pb}), nil
}

func (h SnapshotHandler) ListSnapshots(ctx context.Context, req *connect.Request[ownsandv1.ListSnapshotsRequest]) (*connect.Response[ownsandv1.ListSnapshotsResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	where := []string{"organization_id = $1", "snapshot_id > $2"}
	args := []any{orgID, afterKey(after)}
	if req.Msg.SandboxId != nil {
		if err := validateID("sandbox_id", req.Msg.GetSandboxId()); err != nil {
			return nil, err
		}
		args = append(args, req.Msg.GetSandboxId())
		where = append(where, fmt.Sprintf("source_sandbox_id = $%d", len(args)))
	}
	if req.Msg.DeviceId != nil {
		if err := validateID("device_id", req.Msg.GetDeviceId()); err != nil {
			return nil, err
		}
		args = append(args, req.Msg.GetDeviceId())
		where = append(where, fmt.Sprintf("device_id = $%d", len(args)))
	}
	args = append(args, limit)
	rows, err := h.DB.Pool.Query(ctx, "SELECT "+snapshotColumns+" FROM snapshots WHERE "+strings.Join(where, " AND ")+fmt.Sprintf(" ORDER BY snapshot_id LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, h.internal(ctx, "list snapshots", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListSnapshotsResponse{}
	for rows.Next() {
		sn, err := scanSnapshot(rows)
		if err != nil {
			return nil, h.internal(ctx, "list snapshots", err)
		}
		resp.Snapshots = append(resp.Snapshots, sn.pb)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list snapshots", err)
	}
	last := ""
	if n := len(resp.Snapshots); n > 0 {
		last = resp.Snapshots[n-1].GetSnapshotId()
	}
	resp.NextPageToken = nextPageToken(len(resp.Snapshots), limit, last)
	return connect.NewResponse(resp), nil
}

func (h SnapshotHandler) DeleteSnapshot(ctx context.Context, req *connect.Request[ownsandv1.DeleteSnapshotRequest]) (*connect.Response[ownsandv1.DeleteSnapshotResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		if _, _, err := h.authorize(ctx, tx, orgID, authz.ActionUseSandboxes); err != nil {
			return err
		}
		if err := validateID("snapshot_id", req.Msg.GetSnapshotId()); err != nil {
			return err
		}
		sn, err := loadSnapshot(ctx, tx, orgID, req.Msg.GetSnapshotId(), true)
		if err != nil {
			return err
		}
		var templates bool
		if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM templates WHERE snapshot_id = $1)", sn.pb.GetSnapshotId()).Scan(&templates); err != nil {
			return err
		}
		if templates {
			return errPrecondition("a template references the snapshot")
		}
		return h.deleteSnapshotRecordTx(ctx, tx, orgID, sn.pb.GetSnapshotId())
	})
	if err != nil {
		return nil, h.internal(ctx, "delete snapshot", err)
	}
	h.notify()
	return connect.NewResponse(&ownsandv1.DeleteSnapshotResponse{}), nil
}

func (h SnapshotHandler) ExportSnapshot(ctx context.Context, req *connect.Request[ownsandv1.ExportSnapshotRequest]) (*connect.Response[ownsandv1.ExportSnapshotResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	if err := validateID("snapshot_id", req.Msg.GetSnapshotId()); err != nil {
		return nil, err
	}
	if h.Objects == nil {
		return nil, errPrecondition("snapshot export storage is not configured")
	}
	var sn *snapshotRow
	var conn *hub.Conn
	start := false
	// Every attempt has its own command ID, which the row stores with the
	// ID of the channel that receives the command.
	commandID := newID()
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		var err error
		sn, err = loadSnapshot(ctx, tx, orgID, req.Msg.GetSnapshotId(), true)
		if err != nil {
			return err
		}
		switch sn.pb.GetExportState() {
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED:
			return nil
		case ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING:
			// A healthy export runs on the live channel that owns it, and a
			// repeat call dispatches nothing. Without a live channel the
			// owner cannot be judged and the export stays as it is. A live
			// channel that is not the owner means the export lost its owner
			// without the state report of the new channel having failed it
			// (the channel was replaced while the claim committed): fail the
			// attempt and start a new one.
			live := h.Hub.Live(sn.pb.GetDeviceId())
			if live == nil || live.ID == sn.exportChannelID {
				return nil
			}
			if err := h.failExportTx(ctx, tx, orgID, sn.pb.GetSnapshotId(), sn.exportCommandID, "owner_channel_replaced"); err != nil {
				return err
			}
		}
		if sn.pb.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			return errPrecondition("only a ready snapshot can be exported")
		}
		if sn.pb.GetDeviceId() == "" {
			return errPrecondition("no device holds the snapshot")
		}
		conn = h.Hub.Live(sn.pb.GetDeviceId())
		if conn == nil {
			return errUnavailable("the snapshot's device has no live agent channel")
		}
		sn, err = scanSnapshot(tx.QueryRow(ctx, `UPDATE snapshots SET export_state = $2, export_part_size_bytes = $3, export_command_id = $4, export_channel_id = $5
			WHERE snapshot_id = $1 RETURNING `+snapshotColumns,
			sn.pb.GetSnapshotId(), int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING), h.SnapshotPartSizeBytes, commandID, conn.ID))
		start = err == nil
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "export snapshot", err)
	}
	if start {
		h.Logger.InfoContext(ctx, "export_started", slog.String("snapshot_id", sn.pb.GetSnapshotId()), slog.String("command_id", commandID), slog.String("channel_id", conn.ID))
		if _, err := h.sendCommand(ctx, conn, commandInfo{kind: cmdExportSnapshot, orgID: orgID, snapshotID: sn.pb.GetSnapshotId()},
			&ownsandv1.AgentCommand{CommandId: commandID, Command: &ownsandv1.AgentCommand_ExportSnapshot{ExportSnapshot: &ownsandv1.ExportSnapshotCommand{
				SnapshotId: sn.pb.GetSnapshotId(), PartSizeBytes: h.SnapshotPartSizeBytes,
			}}}); err != nil {
			// The attempt fails only while the row still names it: a
			// concurrent retry owns the row by now and keeps its state.
			tag, execErr := h.DB.Pool.Exec(ctx, "UPDATE snapshots SET export_state = $3 WHERE snapshot_id = $1 AND export_command_id = $2 AND export_state = $4",
				sn.pb.GetSnapshotId(), commandID, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED), int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTING))
			h.Logger.WarnContext(ctx, "export_failed", slog.String("snapshot_id", sn.pb.GetSnapshotId()), slog.String("command_id", commandID),
				slog.String("channel_id", conn.ID), slog.String("reason", "send_failed"), slog.Bool("applied", execErr == nil && tag.RowsAffected() > 0))
			return nil, errUnavailable("the agent channel closed")
		}
	}
	return connect.NewResponse(&ownsandv1.ExportSnapshotResponse{Snapshot: sn.pb}), nil
}

const templateColumns = `template_id::text, organization_id::text, name, snapshot_id::text, created_by_kind, created_by_id, create_time`

func scanTemplate(row pgx.Row) (*ownsandv1.Template, error) {
	var t ownsandv1.Template
	var kind int16
	var createdBy string
	var created time.Time
	if err := row.Scan(&t.TemplateId, &t.OrganizationId, &t.Name, &t.SnapshotId, &kind, &createdBy, &created); err != nil {
		return nil, err
	}
	t.CreatedBy = principalProto(kind, createdBy)
	t.CreateTime = ts(created)
	return &t, nil
}

func (h SnapshotHandler) CreateTemplate(ctx context.Context, req *connect.Request[ownsandv1.CreateTemplateRequest]) (*connect.Response[ownsandv1.CreateTemplateResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	var t *ownsandv1.Template
	err := h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionUseSandboxes)
		if err != nil {
			return err
		}
		if err := validateID("snapshot_id", req.Msg.GetSnapshotId()); err != nil {
			return err
		}
		if err := validateName("name", req.Msg.GetName()); err != nil {
			return err
		}
		// Lock the organization row before the snapshot row, as
		// DeleteOrganization does: the template insert takes FOR KEY SHARE
		// on it through its foreign key.
		if err := lockOrganizationShare(ctx, tx, orgID); err != nil {
			return err
		}
		sn, err := loadSnapshot(ctx, tx, orgID, req.Msg.GetSnapshotId(), true)
		if err != nil {
			return err
		}
		if sn.pb.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
			return errPrecondition("only a ready snapshot can back a template")
		}
		t, err = scanTemplate(tx.QueryRow(ctx, `INSERT INTO templates (template_id, organization_id, name, snapshot_id, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING `+templateColumns, newID(), orgID, req.Msg.GetName(), sn.pb.GetSnapshotId(), int16(p.Kind), p.ID, h.now()))
		if db.IsUniqueViolation(err) {
			return errAlreadyExists("a template with this name exists")
		}
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "create template", err)
	}
	return connect.NewResponse(&ownsandv1.CreateTemplateResponse{Template: t}), nil
}

func (h SnapshotHandler) ListTemplates(ctx context.Context, req *connect.Request[ownsandv1.ListTemplatesRequest]) (*connect.Response[ownsandv1.ListTemplatesResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, "SELECT "+templateColumns+" FROM templates WHERE organization_id = $1 AND template_id > $2 ORDER BY template_id LIMIT $3", orgID, afterKey(after), limit)
	if err != nil {
		return nil, h.internal(ctx, "list templates", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListTemplatesResponse{}
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, h.internal(ctx, "list templates", err)
		}
		resp.Templates = append(resp.Templates, t)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list templates", err)
	}
	last := ""
	if n := len(resp.Templates); n > 0 {
		last = resp.Templates[n-1].GetTemplateId()
	}
	resp.NextPageToken = nextPageToken(len(resp.Templates), limit, last)
	return connect.NewResponse(resp), nil
}

func (h SnapshotHandler) DeleteTemplate(ctx context.Context, req *connect.Request[ownsandv1.DeleteTemplateRequest]) (*connect.Response[ownsandv1.DeleteTemplateResponse], error) {
	orgID := req.Msg.GetOrganizationId()
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	if err := validateID("template_id", req.Msg.GetTemplateId()); err != nil {
		return nil, err
	}
	tag, err := h.DB.Pool.Exec(ctx, "DELETE FROM templates WHERE organization_id = $1 AND template_id = $2", orgID, req.Msg.GetTemplateId())
	if err != nil {
		return nil, h.internal(ctx, "delete template", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, errNotFound("template")
	}
	return connect.NewResponse(&ownsandv1.DeleteTemplateResponse{}), nil
}
