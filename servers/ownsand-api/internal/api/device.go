// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

// DeviceHandler serves DeviceService.
type DeviceHandler struct{ *Service }

var _ ownsandv1connect.DeviceServiceHandler = DeviceHandler{}

// terminalSandboxStates are the stored states that never change again.
var (
	terminalSandboxStates = []int16{
		int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED),
		int16(ownsandv1.SandboxState_SANDBOX_STATE_FAILED),
	}
)

// Enrollment tokens.

const enrollmentTokenColumns = `enrollment_token_id::text, organization_id::text, token_prefix, created_by_kind, created_by_id,
	create_time, expire_time, use_time, COALESCE(device_id::text, ''), revoke_time`

func (s *Service) scanEnrollmentToken(row pgx.Row) (*ownsandv1.EnrollmentToken, error) {
	var t ownsandv1.EnrollmentToken
	var kind int16
	var createdBy string
	var created, expires time.Time
	var used, revoked *time.Time
	if err := row.Scan(&t.EnrollmentTokenId, &t.OrganizationId, &t.TokenPrefix, &kind, &createdBy, &created, &expires, &used, &t.DeviceId, &revoked); err != nil {
		return nil, err
	}
	t.CreatedBy = principalProto(kind, createdBy)
	t.CreateTime = ts(created)
	t.ExpireTime = ts(expires)
	t.UseTime = tsPtr(used)
	switch {
	case used != nil:
		t.State = ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_USED
	case revoked != nil:
		t.State = ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_REVOKED
	case !s.now().Before(expires):
		t.State = ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_EXPIRED
	default:
		t.State = ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_PENDING
	}
	return &t, nil
}

func (h DeviceHandler) CreateEnrollmentToken(ctx context.Context, req *connect.Request[ownsandv1.CreateEnrollmentTokenRequest]) (*connect.Response[ownsandv1.CreateEnrollmentTokenResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	var resp *ownsandv1.CreateEnrollmentTokenResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageDevices)
		if err != nil {
			return err
		}
		if err := h.checkBilling(ctx, tx, orgID, restrictWhenBlocked); err != nil {
			return err
		}
		if err := validate.EnrollmentTokenTTL(req.Msg.GetTtl()); err != nil {
			return errInvalid(err)
		}
		secret, err := auth.NewSecret(auth.SecretEnrollmentToken)
		if err != nil {
			return err
		}
		id := newID()
		now := h.now()
		token, err := h.scanEnrollmentToken(tx.QueryRow(ctx, `INSERT INTO enrollment_tokens (enrollment_token_id, organization_id, secret_sha256, token_prefix, created_by_kind, created_by_id, create_time, expire_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING `+enrollmentTokenColumns,
			id, orgID, secret.Hash, secret.DisplayPrefix, int16(p.Kind), p.ID, now, now.Add(req.Msg.GetTtl().AsDuration())))
		if err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ENROLLMENT_TOKEN_CREATED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ENROLLMENT_TOKEN, TargetID: id,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.CreateEnrollmentTokenResponse{Token: token, EnrollmentToken: secret.Value}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "create enrollment token", err)
	}
	return connect.NewResponse(resp), nil
}

func (h DeviceHandler) ListEnrollmentTokens(ctx context.Context, req *connect.Request[ownsandv1.ListEnrollmentTokensRequest]) (*connect.Response[ownsandv1.ListEnrollmentTokensResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionManageDevices); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	rows, err := h.DB.Pool.Query(ctx, `SELECT `+enrollmentTokenColumns+` FROM enrollment_tokens
		WHERE organization_id = $1 AND enrollment_token_id > $2 ORDER BY enrollment_token_id LIMIT $3`, orgID, afterKey(after), limit)
	if err != nil {
		return nil, h.internal(ctx, "list enrollment tokens", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListEnrollmentTokensResponse{}
	for rows.Next() {
		t, err := h.scanEnrollmentToken(rows)
		if err != nil {
			return nil, h.internal(ctx, "list enrollment tokens", err)
		}
		resp.Tokens = append(resp.Tokens, t)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list enrollment tokens", err)
	}
	last := ""
	if n := len(resp.Tokens); n > 0 {
		last = resp.Tokens[n-1].GetEnrollmentTokenId()
	}
	resp.NextPageToken = nextPageToken(len(resp.Tokens), limit, last)
	return connect.NewResponse(resp), nil
}

func (h DeviceHandler) RevokeEnrollmentToken(ctx context.Context, req *connect.Request[ownsandv1.RevokeEnrollmentTokenRequest]) (*connect.Response[ownsandv1.RevokeEnrollmentTokenResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	rawTokenID := req.Msg.GetEnrollmentTokenId()
	var resp *ownsandv1.RevokeEnrollmentTokenResponse
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageDevices)
		if err != nil {
			return err
		}
		tokenID, err := normalizeID("enrollment_token_id", rawTokenID)
		if err != nil {
			return err
		}
		token, err := h.scanEnrollmentToken(tx.QueryRow(ctx, "SELECT "+enrollmentTokenColumns+" FROM enrollment_tokens WHERE organization_id = $1 AND enrollment_token_id = $2 FOR UPDATE", orgID, tokenID))
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("enrollment token")
		}
		if err != nil {
			return err
		}
		switch token.GetState() {
		case ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_USED:
			return errPrecondition("a used enrollment token cannot be revoked; remove the device instead")
		case ownsandv1.EnrollmentTokenState_ENROLLMENT_TOKEN_STATE_REVOKED:
			resp = &ownsandv1.RevokeEnrollmentTokenResponse{Token: token}
			return nil
		}
		token, err = h.scanEnrollmentToken(tx.QueryRow(ctx, "UPDATE enrollment_tokens SET revoke_time = $3 WHERE organization_id = $1 AND enrollment_token_id = $2 RETURNING "+enrollmentTokenColumns, orgID, tokenID, h.now()))
		if err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_ENROLLMENT_TOKEN_REVOKED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_ENROLLMENT_TOKEN, TargetID: tokenID,
		}); err != nil {
			return err
		}
		resp = &ownsandv1.RevokeEnrollmentTokenResponse{Token: token}
		return nil
	})
	if err != nil {
		return nil, h.internal(ctx, "revoke enrollment token", err)
	}
	return connect.NewResponse(resp), nil
}

// Devices.

// deviceSelect selects a device with its allocation: the resources of its
// CREATING, RUNNING, PAUSED and DELETING sandboxes by stored agent state.
const deviceSelect = `SELECT d.device_id::text, d.organization_id::text, d.name, d.state, d.host_os, d.arch, d.agent_version,
	d.update_state, d.capabilities, d.limit_vcpu_count, d.limit_memory_mib, d.limit_disk_gib, d.vcpu_overcommit_percent,
	d.create_time, d.last_seen_time, d.pending_idle_since,
	COALESCE(a.vcpu, 0), COALESCE(a.memory, 0), COALESCE(a.disk, 0), COALESCE(a.macos, 0)
	FROM devices d LEFT JOIN LATERAL (
		SELECT sum(s.vcpu_count)::bigint AS vcpu, sum(s.memory_mib)::bigint AS memory, sum(s.disk_gib)::bigint AS disk,
			count(*) FILTER (WHERE s.guest_os = 2)::int AS macos
		FROM sandboxes s WHERE s.device_id = d.device_id AND s.agent_state IN (1, 2, 3, 4)
	) a ON true`

// visibleDevice excludes tombstones whose expiry passed: every lookup
// treats them as absent.
const visibleDevice = `(d.tombstone_expire_time IS NULL OR d.tombstone_expire_time > $%d)`

func scanDevice(row pgx.Row) (*ownsandv1.Device, error) {
	var d ownsandv1.Device
	var state, hostOS, arch, update int16
	var caps []byte
	var limits ownsandv1.DeviceLimits
	var created time.Time
	var lastSeen, pendingIdle *time.Time
	var alloc ownsandv1.DeviceAllocation
	var vcpuTotal int64
	if err := row.Scan(&d.DeviceId, &d.OrganizationId, &d.Name, &state, &hostOS, &arch, &d.AgentVersion, &update, &caps,
		&limits.VcpuCount, &limits.MemoryMib, &limits.DiskGib, &limits.VcpuOvercommitPercent,
		&created, &lastSeen, &pendingIdle, &vcpuTotal, &alloc.MemoryMib, &alloc.DiskGib, &alloc.MacosGuestCount); err != nil {
		return nil, err
	}
	d.State = ownsandv1.DeviceState(state)
	d.HostOs = ownsandv1.HostOs(hostOS)
	d.Arch = ownsandv1.Arch(arch)
	d.UpdateState = ownsandv1.AgentUpdateState(update)
	if len(caps) > 0 {
		d.Capabilities = &ownsandv1.DeviceCapabilities{}
		if err := proto.Unmarshal(caps, d.Capabilities); err != nil {
			return nil, fmt.Errorf("decode device capabilities: %w", err)
		}
	}
	d.Limits = &limits
	// The exact value goes in the int64 field. The int32 field stays for v1
	// clients and is capped, because a plain conversion would wrap a valid
	// limit above 2147483647 to a negative number.
	effective := validate.EffectiveVCPULimit(&limits)
	d.EffectiveVcpuLimitCount = effective
	d.EffectiveVcpuLimit = int32(min(effective, math.MaxInt32))
	setAllocationVCPU(&alloc, vcpuTotal)
	d.Allocation = &alloc
	d.CreateTime = ts(created)
	d.LastSeenTime = tsPtr(lastSeen)
	d.PendingIdleSince = tsPtr(pendingIdle)
	return &d, nil
}

// setAllocationVCPU stores the exact allocated vCPUs in the int64 field. The
// int32 field stays for v1 clients and is capped, because a plain conversion
// would wrap a valid total above 2147483647 to a negative number.
func setAllocationVCPU(alloc *ownsandv1.DeviceAllocation, total int64) {
	alloc.VcpuTotalCount = &total
	alloc.VcpuCount = int32(min(total, math.MaxInt32))
}

// loadDevice reads one visible device of the organization.
func (s *Service) loadDevice(ctx context.Context, q db.Querier, orgID, deviceID string) (*ownsandv1.Device, error) {
	d, err := scanDevice(q.QueryRow(ctx, deviceSelect+` WHERE d.organization_id = $1 AND d.device_id = $2 AND `+fmt.Sprintf(visibleDevice, 3), orgID, deviceID, s.now()))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errNotFound("device")
	}
	return d, err
}

func (h DeviceHandler) ListDevices(ctx context.Context, req *connect.Request[ownsandv1.ListDevicesRequest]) (*connect.Response[ownsandv1.ListDevicesResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadDevices); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(req.Msg.GetPageSize(), req.Msg.GetPageToken())
	if err != nil {
		return nil, err
	}
	where := []string{"d.organization_id = $1", "d.device_id > $2", fmt.Sprintf(visibleDevice, 3)}
	args := []any{orgID, afterKey(after), h.now()}
	if req.Msg.State != nil {
		state := req.Msg.GetState()
		if _, ok := ownsandv1.DeviceState_name[int32(state)]; !ok || state == ownsandv1.DeviceState_DEVICE_STATE_UNSPECIFIED {
			return nil, errInvalidf("invalid state: must be a device state")
		}
		args = append(args, int16(state))
		where = append(where, fmt.Sprintf("d.state = $%d", len(args)))
	} else {
		args = append(args, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
		where = append(where, fmt.Sprintf("d.state <> $%d", len(args)))
	}
	args = append(args, limit)
	rows, err := h.DB.Pool.Query(ctx, deviceSelect+" WHERE "+strings.Join(where, " AND ")+fmt.Sprintf(" ORDER BY d.device_id LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, h.internal(ctx, "list devices", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListDevicesResponse{}
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, h.internal(ctx, "list devices", err)
		}
		resp.Devices = append(resp.Devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list devices", err)
	}
	last := ""
	if n := len(resp.Devices); n > 0 {
		last = resp.Devices[n-1].GetDeviceId()
	}
	resp.NextPageToken = nextPageToken(len(resp.Devices), limit, last)
	return connect.NewResponse(resp), nil
}

func (h DeviceHandler) GetDevice(ctx context.Context, req *connect.Request[ownsandv1.GetDeviceRequest]) (*connect.Response[ownsandv1.GetDeviceResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadDevices); err != nil {
		return nil, err
	}
	deviceID, err := normalizeID("device_id", req.Msg.GetDeviceId())
	if err != nil {
		return nil, err
	}
	d, err := h.loadDevice(ctx, h.DB.Pool, orgID, deviceID)
	if err != nil {
		return nil, h.internal(ctx, "get device", err)
	}
	return connect.NewResponse(&ownsandv1.GetDeviceResponse{Device: d}), nil
}

// UpdateDeviceLimits is the legacy RPC with implicit field presence: a
// missing value reads as zero. It shares the write path with
// ReplaceDeviceLimits.
func (h DeviceHandler) UpdateDeviceLimits(ctx context.Context, req *connect.Request[ownsandv1.UpdateDeviceLimitsRequest]) (*connect.Response[ownsandv1.UpdateDeviceLimitsResponse], error) {
	limits := req.Msg.GetLimits()
	device, err := h.setDeviceLimits(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId(), func() (*ownsandv1.DeviceLimits, error) {
		return limits, validate.DeviceLimits(limits)
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.UpdateDeviceLimitsResponse{Device: device}), nil
}

// ReplaceDeviceLimits replaces all four limits. A missing field fails with
// invalid_argument before any write.
func (h DeviceHandler) ReplaceDeviceLimits(ctx context.Context, req *connect.Request[ownsandv1.ReplaceDeviceLimitsRequest]) (*connect.Response[ownsandv1.ReplaceDeviceLimitsResponse], error) {
	device, err := h.setDeviceLimits(ctx, req.Msg.GetOrganizationId(), req.Msg.GetDeviceId(), func() (*ownsandv1.DeviceLimits, error) {
		return validate.DeviceLimitsInput(req.Msg.GetLimits())
	})
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.ReplaceDeviceLimitsResponse{Device: device}), nil
}

// setDeviceLimits is the shared write path of UpdateDeviceLimits and
// ReplaceDeviceLimits: authorization, device ID and limit validation, the
// update, the configuration event and the audit record in one transaction.
// The resolve callback runs after the authorization and ID checks and before
// any write, and returns the validated limits.
func (h DeviceHandler) setDeviceLimits(ctx context.Context, rawOrgID, rawDeviceID string, resolve func() (*ownsandv1.DeviceLimits, error)) (*ownsandv1.Device, error) {
	var device *ownsandv1.Device
	orgID, err := normalizeID("organization_id", rawOrgID)
	if err != nil {
		return nil, err
	}
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageDevices)
		if err != nil {
			return err
		}
		deviceID, err := normalizeID("device_id", rawDeviceID)
		if err != nil {
			return err
		}
		limits, err := resolve()
		if err != nil {
			return errInvalid(err)
		}
		tag, err := tx.Exec(ctx, `UPDATE devices SET limit_vcpu_count = $3, limit_memory_mib = $4, limit_disk_gib = $5, vcpu_overcommit_percent = $6
			WHERE organization_id = $1 AND device_id = $2 AND state <> $7`,
			orgID, deviceID, limits.GetVcpuCount(), limits.GetMemoryMib(), limits.GetDiskGib(), limits.GetVcpuOvercommitPercent(),
			int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errNotFound("device")
		}
		if err := events.Write(ctx, tx, events.Event{Kind: events.KindDeviceConfigChanged, OrganizationID: orgID, DeviceID: deviceID}); err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_LIMITS_CHANGED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_DEVICE, TargetID: deviceID,
		}); err != nil {
			return err
		}
		device, err = h.loadDevice(ctx, tx, orgID, deviceID)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "set device limits", err)
	}
	h.notify()
	return device, nil
}

func (h DeviceHandler) RemoveDevice(ctx context.Context, req *connect.Request[ownsandv1.RemoveDeviceRequest]) (*connect.Response[ownsandv1.RemoveDeviceResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	rawDeviceID := req.Msg.GetDeviceId()
	var device *ownsandv1.Device
	removed := false
	var deviceID string
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageDevices)
		if err != nil {
			return err
		}
		deviceID, err = normalizeID("device_id", rawDeviceID)
		if err != nil {
			return err
		}
		// Lock order: organization row (FOR KEY SHARE), then the device row
		// (FOR UPDATE), then the sandbox rows, as the state report takes
		// them. Concurrent removals of one device serialize on the device
		// row, and the second one finds the tombstone the first one wrote.
		// A deletion of the organization holds the organization row
		// FOR UPDATE, so a removal that waits for it finds no organization.
		var locked int
		err = tx.QueryRow(ctx, "SELECT 1 FROM organizations WHERE organization_id = $1 FOR KEY SHARE", orgID).Scan(&locked)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("organization")
		}
		if err != nil {
			return err
		}
		var state int16
		err = tx.QueryRow(ctx, `SELECT d.state FROM devices d WHERE d.organization_id = $1 AND d.device_id = $2 AND `+fmt.Sprintf(visibleDevice, 3)+` FOR UPDATE OF d`,
			orgID, deviceID, h.now()).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return errNotFound("device")
		}
		if err != nil {
			return err
		}
		if ownsandv1.DeviceState(state) != ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			if err := h.removeDeviceTx(ctx, tx, orgID, deviceID, p, ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DEVICE_REMOVED, true); err != nil {
				return err
			}
			removed = true
		}
		device, err = h.loadDevice(ctx, tx, orgID, deviceID)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "remove device", err)
	}
	if removed {
		h.afterDeviceRemoved(ctx, deviceID)
	}
	return connect.NewResponse(&ownsandv1.RemoveDeviceResponse{Device: device}), nil
}

// removeDeviceTx marks the device removed and keeps its tombstone for one
// year, invalidates its session credentials, ends its non-terminal
// sandboxes with endReason, deletes the records of snapshots that exist
// only on the device (and their templates) and keeps exported snapshots
// without a local copy.
func (s *Service) removeDeviceTx(ctx context.Context, tx pgx.Tx, orgID, deviceID string, actor auth.Principal, endReason ownsandv1.SandboxEndReason, auditDevice bool) error {
	now := s.now()
	// The state guard keeps the first removal's tombstone timestamps and
	// makes a repeated removal write nothing: callers lock the device row
	// first, so this is defensive.
	tag, err := tx.Exec(ctx, `UPDATE devices SET state = $3, remove_time = $4, tombstone_expire_time = $5
		WHERE organization_id = $1 AND device_id = $2 AND state <> $3`,
		orgID, deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED), now, now.Add(tombstoneLifetime))
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, "DELETE FROM device_sessions WHERE device_id = $1", deviceID); err != nil {
		return err
	}
	if err := s.endSandboxesTx(ctx, tx, "device_id = $1", []any{deviceID}, endReason, actor); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DELETE FROM snapshot_copies WHERE device_id = $1", deviceID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "UPDATE snapshots SET device_id = NULL WHERE device_id = $1 AND export_state = $2",
		deviceID, int16(ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED)); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, "SELECT snapshot_id::text FROM snapshots WHERE device_id = $1", deviceID)
	if err != nil {
		return err
	}
	var snaps []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		snaps = append(snaps, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, id := range snaps {
		if err := s.deleteSnapshotRecordTx(ctx, tx, orgID, id); err != nil {
			return err
		}
	}
	if err := events.Write(ctx, tx, events.Event{Kind: events.KindDeviceRemoved, OrganizationID: orgID, DeviceID: deviceID}); err != nil {
		return err
	}
	if !auditDevice {
		return nil
	}
	return s.writeAudit(ctx, tx, auditEvent{
		OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_DEVICE_REMOVED, Actor: actor,
		TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_DEVICE, TargetID: deviceID,
	})
}

// deleteSnapshotRecordTx deletes a snapshot record and the templates that
// reference it. When its export had started, it also starts the R2
// clean-up job of the snapshot prefix, which cancels the export: no signed
// URL is issued for a deleted snapshot.
//
// The job is recorded after the row DELETE: the DELETE waits for every
// signing transaction that holds the row (handleSignedURLRequest takes
// FOR SHARE), so the job's final_sweep_after (now + signedURLLifetime) is
// computed after the last URL that can exist for the prefix was signed and
// is never earlier than its expiry. The export state comes from the
// deleted row (RETURNING), which is current even when the DELETE waited.
func (s *Service) deleteSnapshotRecordTx(ctx context.Context, tx pgx.Tx, orgID, snapshotID string) error {
	// Every device that holds a copy gets a delete-snapshot command from the
	// lease holder; the snapshot_copies rows go with the record.
	rows, err := tx.Query(ctx, `SELECT device_id::text FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2 AND device_id IS NOT NULL
		UNION SELECT device_id::text FROM snapshot_copies WHERE snapshot_id = $2`, orgID, snapshotID)
	if err != nil {
		return err
	}
	var devices []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		devices = append(devices, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	var export int16
	err = tx.QueryRow(ctx, "DELETE FROM snapshots WHERE organization_id = $1 AND snapshot_id = $2 RETURNING export_state", orgID, snapshotID).Scan(&export)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // deleted meanwhile by a transaction that recorded its own job
	}
	if err != nil {
		return err
	}
	if ownsandv1.SnapshotExportState(export) != ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_NOT_EXPORTED {
		if err := s.startCleanupJobTx(ctx, tx, orgID, snapshotPrefix(orgID, snapshotID)); err != nil {
			return err
		}
	}
	for _, d := range devices {
		if err := events.Write(ctx, tx, events.Event{Kind: events.KindSnapshotDeleted, OrganizationID: orgID, DeviceID: d, SnapshotID: snapshotID}); err != nil {
			return err
		}
	}
	return nil
}

// snapshotPrefix is the R2 key prefix of a snapshot's exported objects.
func snapshotPrefix(orgID, snapshotID string) string {
	return organizationKeyPrefix + orgID + snapshotObjectsSegment + snapshotID + "/"
}

// endSandboxesTx ends the non-terminal sandboxes that match where (a
// condition on sandbox columns with its arguments from $1) as DELETED with
// endReason, writes one SANDBOX_DELETED audit event for each and writes a
// sandbox revocation event for each. Sandboxes that are already DELETED or
// FAILED keep their state and end reason.
func (s *Service) endSandboxesTx(ctx context.Context, tx pgx.Tx, where string, args []any, endReason ownsandv1.SandboxEndReason, actor auth.Principal) error {
	n := len(args)
	args = append(args, int16(ownsandv1.SandboxState_SANDBOX_STATE_DELETED), int16(endReason), s.now(), terminalSandboxStates)
	rows, err := tx.Query(ctx, fmt.Sprintf(`UPDATE sandboxes SET agent_state = $%d, end_reason = $%d, end_time = $%d, delete_pending = false
		WHERE %s AND NOT (agent_state = ANY($%d)) RETURNING sandbox_id::text, organization_id::text`, n+1, n+2, n+3, where, n+4), args...)
	if err != nil {
		return err
	}
	type ended struct{ id, org string }
	var all []ended
	for rows.Next() {
		var e ended
		if err := rows.Scan(&e.id, &e.org); err != nil {
			rows.Close()
			return err
		}
		all = append(all, e)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, e := range all {
		if err := s.writeAudit(ctx, tx, auditEvent{
			OrganizationID: e.org, Action: ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_DELETED, Actor: actor, EndReason: endReason,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_SANDBOX, TargetID: e.id,
		}); err != nil {
			return err
		}
		if err := events.Write(ctx, tx, events.Event{Kind: events.KindSandboxDeleted, OrganizationID: e.org, SandboxID: e.id}); err != nil {
			return err
		}
	}
	return nil
}

// Agent version policy.

func (s *Service) loadPolicy(ctx context.Context, q db.Querier, orgID string) (*ownsandv1.AgentVersionPolicy, error) {
	var mode, kind int16
	var pinned, updatedBy string
	var updated time.Time
	err := q.QueryRow(ctx, "SELECT mode, pinned_version, update_time, updated_by_kind, updated_by_id FROM agent_version_policies WHERE organization_id = $1", orgID).
		Scan(&mode, &pinned, &updated, &kind, &updatedBy)
	if errors.Is(err, pgx.ErrNoRows) {
		return &ownsandv1.AgentVersionPolicy{OrganizationId: orgID, Mode: ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT}, nil
	}
	if err != nil {
		return nil, err
	}
	return &ownsandv1.AgentVersionPolicy{
		OrganizationId: orgID, Mode: ownsandv1.AgentVersionPolicyMode(mode), PinnedVersion: pinned,
		UpdateTime: ts(updated), UpdatedBy: principalProto(kind, updatedBy),
	}, nil
}

func (h DeviceHandler) GetAgentVersionPolicy(ctx context.Context, req *connect.Request[ownsandv1.GetAgentVersionPolicyRequest]) (*connect.Response[ownsandv1.GetAgentVersionPolicyResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionReadDevices); err != nil {
		return nil, err
	}
	policy, err := h.loadPolicy(ctx, h.DB.Pool, orgID)
	if err != nil {
		return nil, h.internal(ctx, "get agent version policy", err)
	}
	return connect.NewResponse(&ownsandv1.GetAgentVersionPolicyResponse{Policy: policy}), nil
}

// maxVersionLength bounds a pinned version string.
const maxVersionLength = 64

func (h DeviceHandler) UpdateAgentVersionPolicy(ctx context.Context, req *connect.Request[ownsandv1.UpdateAgentVersionPolicyRequest]) (*connect.Response[ownsandv1.UpdateAgentVersionPolicyResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	mode := req.Msg.GetMode()
	pinned := req.Msg.GetPinnedVersion()
	var policy *ownsandv1.AgentVersionPolicy
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionManageDevices)
		if err != nil {
			return err
		}
		switch mode {
		case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_FOLLOW_DEFAULT, ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD:
			if pinned != "" {
				return errInvalidf("invalid pinned_version: must be empty unless the mode is PIN")
			}
		case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN:
			if pinned == "" || len(pinned) > maxVersionLength || strings.ContainsAny(pinned, " \t\r\n") {
				return errInvalidf("invalid pinned_version: a version is required for PIN")
			}
			// PostgreSQL text cannot store U+0000.
			if err := validate.NoNUL("pinned_version", pinned); err != nil {
				return errInvalid(err)
			}
			var revoked bool
			if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_releases WHERE version = $1 AND revoked)", pinned).Scan(&revoked); err != nil {
				return err
			}
			if revoked {
				return errInvalidf("invalid pinned_version: the version is revoked")
			}
		default:
			return errInvalidf("invalid mode: must be FOLLOW_DEFAULT, PIN or HOLD")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO agent_version_policies (organization_id, mode, pinned_version, update_time, updated_by_kind, updated_by_id)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (organization_id) DO UPDATE SET mode = EXCLUDED.mode, pinned_version = EXCLUDED.pinned_version,
				update_time = EXCLUDED.update_time, updated_by_kind = EXCLUDED.updated_by_kind, updated_by_id = EXCLUDED.updated_by_id`,
			orgID, int16(mode), pinned, h.now(), int16(p.Kind), p.ID); err != nil {
			return err
		}
		if err := events.Write(ctx, tx, events.Event{Kind: events.KindAgentPolicyChanged, OrganizationID: orgID}); err != nil {
			return err
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_AGENT_VERSION_POLICY_CHANGED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_AGENT_VERSION_POLICY, TargetID: orgID,
		}); err != nil {
			return err
		}
		policy, err = h.loadPolicy(ctx, tx, orgID)
		return err
	})
	if err != nil {
		return nil, h.internal(ctx, "update agent version policy", err)
	}
	h.notify()
	return connect.NewResponse(&ownsandv1.UpdateAgentVersionPolicyResponse{Policy: policy}), nil
}
