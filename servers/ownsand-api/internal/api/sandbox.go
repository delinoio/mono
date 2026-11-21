// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
	"github.com/delinoio/mono/servers/ownsand-api/internal/scheduler"
)

// SandboxHandler serves SandboxService.
type SandboxHandler struct{ *Service }

var _ ownsandv1connect.SandboxServiceHandler = SandboxHandler{}

// idempotencyLifetime is how long a create idempotency key is kept.
const idempotencyLifetime = 24 * time.Hour

// guestOfBaseImage maps a base image to its guest OS.
func guestOfBaseImage(b ownsandv1.BaseImage) ownsandv1.GuestOs {
	switch b {
	case ownsandv1.BaseImage_BASE_IMAGE_LINUX:
		return ownsandv1.GuestOs_GUEST_OS_LINUX
	case ownsandv1.BaseImage_BASE_IMAGE_MACOS:
		return ownsandv1.GuestOs_GUEST_OS_MACOS
	case ownsandv1.BaseImage_BASE_IMAGE_WINDOWS:
		return ownsandv1.GuestOs_GUEST_OS_WINDOWS
	}
	return ownsandv1.GuestOs_GUEST_OS_UNSPECIFIED
}

// requestHash is the SHA-256 of the deterministic encoding of a create
// request, which the idempotency key binds.
func requestHash(req *ownsandv1.CreateSandboxRequest) ([]byte, error) {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(req)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(b)
	return sum[:], nil
}

// canonicalizeCreateSandbox rewrites the identifiers of a validated create
// request to their lowercase text form in place. The API accepts either hex
// case, but the request hash, the idempotency lock, the scheduler's device
// filter and the stored records compare identifiers as text, so one request
// spelled in two cases must read as one request.
func canonicalizeCreateSandbox(m *ownsandv1.CreateSandboxRequest) {
	m.OrganizationId = canonicalID(m.GetOrganizationId())
	m.IdempotencyKey = canonicalID(m.GetIdempotencyKey())
	if src := m.GetSource(); src != nil {
		switch s := src.GetSource().(type) {
		case *ownsandv1.SandboxSource_SnapshotId:
			s.SnapshotId = canonicalID(s.SnapshotId)
		case *ownsandv1.SandboxSource_TemplateId:
			s.TemplateId = canonicalID(s.TemplateId)
		}
	}
	if p := m.GetPlacement(); p != nil && p.DeviceId != nil {
		p.DeviceId = proto.String(canonicalID(p.GetDeviceId()))
	}
}

// lookupIdempotent returns the sandbox of a known idempotency key, or nil.
// A known key with other parameters fails with already_exists.
func (s *Service) lookupIdempotent(ctx context.Context, q db.Querier, orgID, key string, hash []byte) (*sandboxRow, error) {
	var stored []byte
	var sandboxID string
	err := q.QueryRow(ctx, "SELECT request_sha256, sandbox_id::text FROM sandbox_create_keys WHERE organization_id = $1 AND idempotency_key = $2 AND expire_time > $3",
		orgID, key, s.now()).Scan(&stored, &sandboxID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !auth.HashEqual(stored, hash) {
		return nil, errAlreadyExists("the idempotency_key was used with other parameters")
	}
	return loadSandbox(ctx, q, orgID, sandboxID, false)
}

// placementDevices loads every non-removed device of the organization for
// the scheduler. The caller holds row locks on them.
func (s *Service) placementDevices(ctx context.Context, tx pgx.Tx, orgID string, lineage string) ([]scheduler.Device, error) {
	if _, err := tx.Exec(ctx, "SELECT 1 FROM devices WHERE organization_id = $1 AND state <> $2 FOR UPDATE",
		orgID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)); err != nil {
		return nil, err
	}
	reserved := false
	if lineage != "" {
		// The identifier is reserved while any sandbox that uses it may still
		// run: a sandbox in an active stored state, or one that ended because
		// its device was removed while the removal is unconfirmed, until its
		// max_ttl deadline.
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM sandboxes s WHERE s.machine_lineage = $1 AND s.max_deadline > $2 AND (
			s.agent_state IN (1, 2, 3, 4)
			OR (s.end_reason IN (4, 5) AND EXISTS (SELECT 1 FROM devices d WHERE d.device_id = s.device_id AND d.state = 3))))`,
			lineage, s.now()).Scan(&reserved)
		if err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT d.device_id::text, d.state, d.host_os, d.arch, d.capabilities, d.limit_vcpu_count, d.limit_memory_mib, d.limit_disk_gib,
		d.vcpu_overcommit_percent, d.update_state, d.pending_idle_since, `+versionRevokedQuery()+`,
		COALESCE(a.vcpu, 0), COALESCE(a.memory, 0), COALESCE(a.disk, 0), COALESCE(a.macos, 0), COALESCE(a.unreported, 0)
		FROM devices d LEFT JOIN LATERAL (
			SELECT sum(s.vcpu_count)::int AS vcpu, sum(s.memory_mib)::bigint AS memory, sum(s.disk_gib)::bigint AS disk,
				count(*) FILTER (WHERE s.guest_os = 2)::int AS macos,
				COALESCE(sum(s.disk_gib) FILTER (WHERE s.agent_state = 1 AND s.revision_epoch = ''), 0)::bigint AS unreported
			FROM sandboxes s WHERE s.device_id = d.device_id AND s.agent_state IN (1, 2, 3, 4)
		) a ON true
		WHERE d.organization_id = $1 AND d.state <> $2 ORDER BY d.device_id`, orgID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []scheduler.Device
	for rows.Next() {
		var d scheduler.Device
		var state, host, arch, update int16
		var caps []byte
		limits := &ownsandv1.DeviceLimits{}
		alloc := &ownsandv1.DeviceAllocation{}
		if err := rows.Scan(&d.ID, &state, &host, &arch, &caps, &limits.VcpuCount, &limits.MemoryMib, &limits.DiskGib, &limits.VcpuOvercommitPercent,
			&update, &d.PendingIdleSince, &d.VersionRevoked, &alloc.VcpuCount, &alloc.MemoryMib, &alloc.DiskGib, &alloc.MacosGuestCount, &d.UnreportedCreatingDiskGiB); err != nil {
			return nil, err
		}
		d.Online = ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_ONLINE
		d.LiveChannel = s.Hub != nil && s.Hub.Live(d.ID) != nil
		d.HostOS, d.Arch = ownsandv1.HostOs(host), ownsandv1.Arch(arch)
		if len(caps) > 0 {
			d.Capabilities = &ownsandv1.DeviceCapabilities{}
			if err := proto.Unmarshal(caps, d.Capabilities); err != nil {
				return nil, err
			}
		}
		d.Limits, d.Allocation = limits, alloc
		d.UpdateState = ownsandv1.AgentUpdateState(update)
		d.MachineIdentifierReserved = reserved
		out = append(out, d)
	}
	return out, rows.Err()
}

// resolvedSource is the snapshot behind a snapshot or template source.
type resolvedSource struct {
	snapshot   *snapshotRow
	templateID string
	copies     []string
	parts      []*ownsandv1.SnapshotPart
}

func (s *Service) resolveSource(ctx context.Context, q db.Querier, orgID string, src *ownsandv1.SandboxSource) (*resolvedSource, error) {
	var res resolvedSource
	snapshotID := src.GetSnapshotId()
	if src.GetTemplateId() != "" {
		res.templateID = src.GetTemplateId()
		err := q.QueryRow(ctx, "SELECT snapshot_id::text FROM templates WHERE organization_id = $1 AND template_id = $2", orgID, res.templateID).Scan(&snapshotID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, errNotFound("template")
		}
		if err != nil {
			return nil, err
		}
	}
	if snapshotID == "" {
		return nil, nil
	}
	sn, err := loadSnapshot(ctx, q, orgID, snapshotID, false)
	if err != nil {
		return nil, err
	}
	if sn.pb.GetState() != ownsandv1.SnapshotState_SNAPSHOT_STATE_READY {
		return nil, errPrecondition("the snapshot is not ready")
	}
	res.snapshot = sn
	rows, err := q.Query(ctx, "SELECT device_id::text FROM snapshot_copies WHERE snapshot_id = $1", snapshotID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		res.copies = append(res.copies, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	partRows, err := q.Query(ctx, "SELECT part_number, size_bytes, sha256 FROM snapshot_parts WHERE snapshot_id = $1 ORDER BY part_number", snapshotID)
	if err != nil {
		return nil, err
	}
	defer partRows.Close()
	for partRows.Next() {
		var p ownsandv1.SnapshotPart
		if err := partRows.Scan(&p.PartNumber, &p.SizeBytes, &p.Sha256); err != nil {
			return nil, err
		}
		res.parts = append(res.parts, &p)
	}
	return &res, partRows.Err()
}

func schedulerCode(err error) error {
	var se *scheduler.Error
	if !errors.As(err, &se) {
		return err
	}
	return connect.NewError(connect.Code(se.Code), errors.New(se.Reason))
}

func (h SandboxHandler) CreateSandbox(ctx context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	m := req.Msg
	orgID := m.GetOrganizationId()
	p, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes)
	if err != nil {
		return nil, err
	}
	if err := validate.CreateSandboxRequest(m); err != nil {
		return nil, errInvalid(err)
	}
	// Every identifier is valid now: use one spelling for the request hash,
	// the idempotency lock, the scheduler and the records.
	canonicalizeCreateSandbox(m)
	orgID = m.GetOrganizationId()
	network, _ := validate.NetworkMode(m.GetNetwork())
	hash, err := requestHash(m)
	if err != nil {
		return nil, h.internal(ctx, "hash create request", err)
	}
	// The idempotency key is checked right after authorization and before
	// placement or the live-channel check.
	if existing, err := h.lookupIdempotent(ctx, h.DB.Pool, orgID, m.GetIdempotencyKey(), hash); err != nil || existing != nil {
		if err != nil {
			return nil, h.internal(ctx, "create sandbox", err)
		}
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: existing.proto()}), nil
	}
	sandboxID := newID()
	now := h.now()
	var src *resolvedSource
	var sreq scheduler.Request
	var created, replayed *sandboxRow
	var decision scheduler.Decision
	var limits *ownsandv1.DeviceLimits
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		// A miss on the pool is not final: an identical request may be
		// reserving the same key right now. Serialize on the key, then look
		// it up again in this transaction before anything that can fail with
		// a capacity error. The lock is transaction-scoped and keyed by the
		// organization and key; orgID is a fixed-length UUID, so the joined
		// text is unambiguous. Only this handler takes it, and it never waits
		// for it while holding a device row lock (the device lock comes
		// later in this transaction), so the lock order stays acyclic.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1::text || ':' || $2::text, 0))", orgID, m.GetIdempotencyKey()); err != nil {
			return err
		}
		existing, err := h.lookupIdempotent(ctx, tx, orgID, m.GetIdempotencyKey(), hash)
		if err != nil || existing != nil {
			// Another request committed the key first: return its sandbox
			// without a reservation, audit event or command.
			replayed = existing
			return err
		}
		// Lock the organization row before any device row, as
		// DeleteOrganization does: the sandbox insert below takes FOR KEY
		// SHARE on it through its foreign key.
		if err := lockOrganizationShare(ctx, tx, orgID); err != nil {
			return err
		}
		if src, err = h.resolveSource(ctx, tx, orgID, m.GetSource()); err != nil {
			return err
		}
		sreq = scheduler.Request{
			VCPUCount: m.GetResources().GetVcpuCount(), MemoryMiB: m.GetResources().GetMemoryMib(), DiskGiB: m.GetResources().GetDiskGib(),
			NetworkMode: network, BaseImage: m.GetSource().GetBaseImage(),
			DeviceID: m.GetPlacement().GetDeviceId(), HostOS: m.GetPlacement().GetHostOs(), Arch: m.GetPlacement().GetArch(),
		}
		lineage := ""
		if src != nil {
			sn := src.snapshot.pb
			if m.GetResources().GetDiskGib() < sn.GetResources().GetDiskGib() {
				return errInvalidf("invalid resources.disk_gib: must be at least the snapshot's disk size")
			}
			sreq.GuestOS = sn.GetGuestOs()
			sreq.Snapshot = &scheduler.Snapshot{
				ID: sn.GetSnapshotId(), DeviceID: sn.GetDeviceId(), CopyDeviceIDs: src.copies,
				Exported: sn.GetExportState() == ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORTED,
				HostOS:   sn.GetHostOs(), Arch: sn.GetArch(), GuestOS: sn.GetGuestOs(), VCPUCount: sn.GetResources().GetVcpuCount(),
				MemoryMiB: sn.GetResources().GetMemoryMib(), NetworkMode: sn.GetNetworkMode(), HasMemoryState: sn.GetHasMemoryState(),
				MacOSHardwareModel: sn.GetMacosHardwareModel(),
			}
			if sn.GetGuestOs() == ownsandv1.GuestOs_GUEST_OS_MACOS {
				lineage = src.snapshot.lineage
			}
		} else {
			sreq.GuestOS = guestOfBaseImage(sreq.BaseImage)
		}
		devices, err := h.placementDevices(ctx, tx, orgID, lineage)
		if err != nil {
			return err
		}
		decision, err = scheduler.Place(sreq, devices, now)
		if err != nil {
			return schedulerCode(err)
		}
		var hostOS ownsandv1.HostOs
		var arch ownsandv1.Arch
		for _, d := range devices {
			if d.ID == decision.DeviceID {
				limits, hostOS, arch = d.Limits, d.HostOS, d.Arch
			}
		}
		labels, err := json.Marshal(nonNil(m.GetLabels()))
		if err != nil {
			return err
		}
		var base, srcSnapshot, srcTemplate, resolved, machine any
		if src == nil {
			base = int16(sreq.BaseImage)
			if sreq.GuestOS == ownsandv1.GuestOs_GUEST_OS_MACOS {
				machine = sandboxID
			}
		} else {
			resolved = src.snapshot.pb.GetSnapshotId()
			if src.templateID != "" {
				srcTemplate = src.templateID
			} else {
				srcSnapshot = src.snapshot.pb.GetSnapshotId()
			}
			if lineage != "" {
				machine = lineage
			}
		}
		idle, maxTTL := m.GetIdleTtl().AsDuration(), m.GetMaxTtl().AsDuration()
		if _, err := tx.Exec(ctx, `INSERT INTO sandboxes (sandbox_id, organization_id, device_id, agent_state, source_base_image, source_snapshot_id,
			source_template_id, resolved_snapshot_id, vcpu_count, memory_mib, disk_gib, guest_os, host_os, arch, network_mode, labels,
			idle_ttl_ms, max_ttl_ms, idle_deadline, max_deadline, machine_lineage, created_by_kind, created_by_id, create_time)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24)`,
			sandboxID, orgID, decision.DeviceID, int16(ownsandv1.SandboxState_SANDBOX_STATE_CREATING), base, srcSnapshot, srcTemplate, resolved,
			sreq.VCPUCount, sreq.MemoryMiB, sreq.DiskGiB, int16(sreq.GuestOS), int16(hostOS), int16(arch), int16(network), labels,
			idle.Milliseconds(), maxTTL.Milliseconds(), now.Add(idle), now.Add(maxTTL), machine, int16(p.Kind), p.ID, now); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `INSERT INTO sandbox_create_keys (organization_id, idempotency_key, request_sha256, sandbox_id, expire_time)
			VALUES ($1, $2, $3, $4, $5) ON CONFLICT (organization_id, idempotency_key) DO UPDATE
			SET request_sha256 = EXCLUDED.request_sha256, sandbox_id = EXCLUDED.sandbox_id, expire_time = EXCLUDED.expire_time
			WHERE sandbox_create_keys.expire_time <= $6`,
			orgID, m.GetIdempotencyKey(), hash, sandboxID, now.Add(idempotencyLifetime), now)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errIdempotencyRace
		}
		if err := h.writeAudit(ctx, tx, auditEvent{
			OrganizationID: orgID, Action: ownsandv1.AuditAction_AUDIT_ACTION_SANDBOX_CREATED, Actor: p,
			TargetKind: ownsandv1.AuditTargetKind_AUDIT_TARGET_KIND_SANDBOX, TargetID: sandboxID,
		}); err != nil {
			return err
		}
		created, err = loadSandbox(ctx, tx, orgID, sandboxID, false)
		return err
	})
	if errors.Is(err, errIdempotencyRace) {
		// A concurrent request with the same key committed first.
		existing, lerr := h.lookupIdempotent(ctx, h.DB.Pool, orgID, m.GetIdempotencyKey(), hash)
		if lerr != nil || existing == nil {
			return nil, h.internal(ctx, "create sandbox", errors.Join(err, lerr))
		}
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: existing.proto()}), nil
	}
	if err != nil {
		return nil, h.internal(ctx, "create sandbox", err)
	}
	if replayed != nil {
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: replayed.proto()}), nil
	}
	h.Logger.InfoContext(ctx, "sandbox_created", slog.String("sandbox_id", sandboxID), slog.String("device_id", decision.DeviceID),
		slog.Bool("memory_restore", decision.MemoryRestore), slog.Bool("import", decision.NeedsImport))

	cmd := &ownsandv1.CreateSandboxCommand{
		SandboxId:            sandboxID,
		Resources:            &ownsandv1.Resources{VcpuCount: sreq.VCPUCount, MemoryMib: sreq.MemoryMiB, DiskGib: sreq.DiskGiB},
		NetworkMode:          network,
		GuestOs:              sreq.GuestOS,
		IdleTtl:              m.GetIdleTtl(),
		MaxTtl:               m.GetMaxTtl(),
		CreateTime:           ts(now),
		MaxDeadline:          ts(now.Add(m.GetMaxTtl().AsDuration())),
		Limits:               limits,
		MemoryRestoreAllowed: decision.MemoryRestore,
	}
	if src == nil {
		cmd.Source = &ownsandv1.CreateSandboxCommand_BaseImage{BaseImage: sreq.BaseImage}
	} else {
		cmd.Source = &ownsandv1.CreateSandboxCommand_SnapshotId{SnapshotId: src.snapshot.pb.GetSnapshotId()}
	}
	conn := h.Hub.Live(decision.DeviceID)
	if conn == nil {
		// The channel closed after placement. The sandbox stays CREATING
		// until the device's next state report, which reports it as lost.
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: created.proto()}), nil
	}
	info := commandInfo{kind: cmdCreateSandbox, orgID: orgID, sandboxID: sandboxID}
	if decision.NeedsImport {
		go h.importThenCreate(conn, info, src, cmd)
	} else if _, err := h.sendCommand(ctx, conn, info, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: cmd}}); err != nil {
		h.Logger.WarnContext(ctx, "agent_command_send_failed", slog.String("sandbox_id", sandboxID), slog.Any("error", err))
	}
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: created.proto()}), nil
}

var errIdempotencyRace = errors.New("idempotency key race")

func nonNil(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// importThenCreate imports an exported snapshot on the selected device and
// then sends the create command. A failed import fails the create.
func (s *Service) importThenCreate(conn *hub.Conn, info commandInfo, src *resolvedSource, cmd *ownsandv1.CreateSandboxCommand) {
	ctx, cancel := context.WithTimeout(conn.Context(), s.Timing.ImportWait)
	defer cancel()
	sn := src.snapshot.pb
	importCmd := &ownsandv1.AgentCommand{CommandId: newID(), Command: &ownsandv1.AgentCommand_ImportSnapshot{ImportSnapshot: &ownsandv1.ImportSnapshotCommand{
		Snapshot: &ownsandv1.SnapshotStateEntry{
			SnapshotId: sn.GetSnapshotId(), SourceSandboxId: sn.GetSourceSandboxId(), State: sn.GetState(), GuestOs: sn.GetGuestOs(),
			Resources: sn.GetResources(), NetworkMode: sn.GetNetworkMode(), HasMemoryState: sn.GetHasMemoryState(),
			SizeBytes: sn.GetSizeBytes(), MacosHardwareModel: sn.GetMacosHardwareModel(),
		},
		Parts: src.parts,
	}}}
	wait := s.Hub.Commands.Register(importCmd.CommandId)
	defer s.Hub.Commands.Cancel(importCmd.CommandId)
	// The assignment is recorded before the command is sent, so the agent's
	// first download URL request finds it. It is cleared when the import
	// ends: on a result, a send failure, the wait timeout or a closed channel
	// (every return below) and as soon as the result arrives.
	releaseImport := s.imports.register(conn, sn.GetSnapshotId(), importCmd.CommandId)
	defer releaseImport()
	if _, err := s.sendCommand(ctx, conn, commandInfo{kind: cmdImportSnapshot, orgID: info.orgID, snapshotID: sn.GetSnapshotId()}, importCmd); err != nil {
		s.failImportSendDeadline(ctx, conn, info, err)
		return
	}
	select {
	case res := <-wait:
		releaseImport()
		if res.GetError() != nil {
			_ = s.failCreateRejected(context.WithoutCancel(ctx), info.sandboxID)
			s.watch.fire(info.sandboxID)
			return
		}
	case <-ctx.Done():
		// A closed channel keeps reconnect reconciliation as the owner of the
		// sandbox. An expired wait on a live connection would otherwise leave
		// the CREATING reservation committed forever, so fail the create.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			s.failImportTimeout(ctx, conn, info)
		}
		return
	}
	if _, err := s.sendCommand(ctx, conn, info, &ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_CreateSandbox{CreateSandbox: cmd}}); err != nil {
		s.Logger.WarnContext(ctx, "agent_command_send_failed", slog.String("sandbox_id", info.sandboxID), slog.Any("error", err))
		s.failImportSendDeadline(ctx, conn, info, err)
	}
}

// failImportSendDeadline fails the create when a send of the import flow
// returned because the import deadline expired while the channel stayed
// connected (a full outbound queue held the send until then). Other send
// errors keep reconnect reconciliation as the owner of the sandbox: a closed
// channel, including one closed by a queue that stayed full, is reconciled
// when the device reconnects.
func (s *Service) failImportSendDeadline(ctx context.Context, conn *hub.Conn, info commandInfo, err error) {
	if errors.Is(err, context.DeadlineExceeded) && conn.Context().Err() == nil {
		s.failImportTimeout(ctx, conn, info)
	}
}

// importFailTimeout bounds the database update that fails a create whose
// import wait expired; the expired wait context cannot be used for it.
const importFailTimeout = 5 * time.Second

// failImportTimeout moves a sandbox whose import wait expired to FAILED with
// CREATE_FAILED through the guarded rejection transition and notifies
// watchers. It does not retry the import. A late import success cannot revive
// the sandbox because the transition only acts on CREATING and the create
// command is never sent after the import goroutine returned.
func (s *Service) failImportTimeout(ctx context.Context, conn *hub.Conn, info commandInfo) {
	dbCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), importFailTimeout)
	defer cancel()
	err := s.failCreateRejected(dbCtx, info.sandboxID)
	attrs := []any{
		slog.String("sandbox_id", info.sandboxID),
		slog.Duration("import_wait", s.Timing.ImportWait),
	}
	if err != nil {
		s.Logger.ErrorContext(dbCtx, "import_timeout_fail_create_failed", append(attrs, slog.Any("error", err))...)
		return
	}
	s.Logger.WarnContext(dbCtx, "import_timeout_create_failed", attrs...)
	s.watch.fire(info.sandboxID)
}

func (h SandboxHandler) GetSandbox(ctx context.Context, req *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	sandboxID, err := normalizeID("sandbox_id", req.Msg.GetSandboxId())
	if err != nil {
		return nil, err
	}
	sb, err := loadSandbox(ctx, h.DB.Pool, orgID, sandboxID, false)
	if err != nil {
		return nil, h.internal(ctx, "get sandbox", err)
	}
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: sb.proto()}), nil
}

// shownStateSQL computes the shown state of a sandbox row in SQL, like
// sandboxRow.shownState.
const shownStateSQL = `(CASE WHEN s.agent_state IN (5, 6) THEN s.agent_state WHEN COALESCE(d.state, 3) <> 1 THEN 7
	WHEN s.delete_pending THEN 4 ELSE s.agent_state END)`

func (h SandboxHandler) ListSandboxes(ctx context.Context, req *connect.Request[ownsandv1.ListSandboxesRequest]) (*connect.Response[ownsandv1.ListSandboxesResponse], error) {
	m := req.Msg
	orgID, err := normalizeID("organization_id", m.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	limit, after, err := pageArgs(m.GetPageSize(), m.GetPageToken())
	if err != nil {
		return nil, err
	}
	where := []string{"s.organization_id = $1", "s.sandbox_id > $2"}
	args := []any{orgID, afterKey(after)}
	if m.DeviceId != nil {
		deviceID, err := normalizeID("device_id", m.GetDeviceId())
		if err != nil {
			return nil, err
		}
		args = append(args, deviceID)
		where = append(where, fmt.Sprintf("s.device_id = $%d", len(args)))
	}
	if m.State != nil {
		if _, ok := ownsandv1.SandboxState_name[int32(m.GetState())]; !ok || m.GetState() == ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED {
			return nil, errInvalidf("invalid state: must be a sandbox state")
		}
		args = append(args, int16(m.GetState()))
		where = append(where, fmt.Sprintf("%s = $%d", shownStateSQL, len(args)))
	} else if !m.GetIncludeEnded() {
		where = append(where, "s.agent_state NOT IN (5, 6)")
	}
	args = append(args, limit)
	rows, err := h.DB.Pool.Query(ctx, sandboxSelect+" WHERE "+strings.Join(where, " AND ")+fmt.Sprintf(" ORDER BY s.sandbox_id LIMIT $%d", len(args)), args...)
	if err != nil {
		return nil, h.internal(ctx, "list sandboxes", err)
	}
	defer rows.Close()
	resp := &ownsandv1.ListSandboxesResponse{}
	for rows.Next() {
		sb, err := scanSandbox(rows)
		if err != nil {
			return nil, h.internal(ctx, "list sandboxes", err)
		}
		resp.Sandboxes = append(resp.Sandboxes, sb.proto())
	}
	if err := rows.Err(); err != nil {
		return nil, h.internal(ctx, "list sandboxes", err)
	}
	last := ""
	if n := len(resp.Sandboxes); n > 0 {
		last = resp.Sandboxes[n-1].GetSandboxId()
	}
	resp.NextPageToken = nextPageToken(len(resp.Sandboxes), limit, last)
	return connect.NewResponse(resp), nil
}

// waitSandbox waits up to the command wait for done to report true on the
// sandbox, and returns the current sandbox either way.
func (s *Service) waitSandbox(ctx context.Context, orgID, sandboxID string, done func(*sandboxRow) bool) (*sandboxRow, error) {
	deadline := time.NewTimer(s.Timing.CommandWait)
	defer deadline.Stop()
	// Other instances change sandboxes too, so poll as a fallback to the
	// in-process notification.
	poll := time.NewTicker(time.Second)
	defer poll.Stop()
	for {
		sb, err, finished := s.waitSandboxOnce(ctx, orgID, sandboxID, done, poll.C, deadline.C)
		if finished {
			return sb, err
		}
	}
}

// waitSandboxOnce is one iteration of waitSandbox. It subscribes before it
// reads, so a change between the read and the wait is not lost, and it
// unregisters the subscription on every exit.
func (s *Service) waitSandboxOnce(ctx context.Context, orgID, sandboxID string, done func(*sandboxRow) bool, poll, deadline <-chan time.Time) (sb *sandboxRow, err error, finished bool) {
	changed, unregister := s.watch.watch(sandboxID)
	defer unregister()
	sb, err = loadSandbox(ctx, s.DB.Pool, orgID, sandboxID, false)
	if err != nil || done(sb) {
		return sb, err, true
	}
	select {
	case <-changed:
		return nil, nil, false
	case <-poll:
		return nil, nil, false
	case <-deadline:
		sb, err = loadSandbox(ctx, s.DB.Pool, orgID, sandboxID, false)
		return sb, err, true
	case <-ctx.Done():
		return nil, ctx.Err(), true
	}
}

func (h SandboxHandler) DeleteSandbox(ctx context.Context, req *connect.Request[ownsandv1.DeleteSandboxRequest]) (*connect.Response[ownsandv1.DeleteSandboxResponse], error) {
	orgID, err := normalizeID("organization_id", req.Msg.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	rawSandboxID := req.Msg.GetSandboxId()
	var sb *sandboxRow
	var sandboxID string
	err = h.DB.InTx(ctx, func(tx pgx.Tx) error {
		p, _, err := h.authorize(ctx, tx, orgID, authz.ActionUseSandboxes)
		if err != nil {
			return err
		}
		sandboxID, err = normalizeID("sandbox_id", rawSandboxID)
		if err != nil {
			return err
		}
		sb, err = loadSandbox(ctx, tx, orgID, sandboxID, true)
		if err != nil || isTerminal(sb.AgentState) || sb.DeletePending {
			return err
		}
		// Any instance records the pending delete; the lease holder sends the
		// delete command, at once or when the device reconnects.
		if _, err := tx.Exec(ctx, `UPDATE sandboxes SET delete_pending = true, delete_end_reason = $2, delete_actor_kind = $3, delete_actor_id = $4
			WHERE sandbox_id = $1`, sandboxID, int16(ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER), int16(p.Kind), p.ID); err != nil {
			return err
		}
		return events.Write(ctx, tx, events.Event{Kind: events.KindSandboxDeleteRequested, OrganizationID: orgID, SandboxID: sandboxID, DeviceID: sb.DeviceID})
	})
	if err != nil {
		return nil, h.internal(ctx, "delete sandbox", err)
	}
	if isTerminal(sb.AgentState) {
		return connect.NewResponse(&ownsandv1.DeleteSandboxResponse{Sandbox: sb.proto()}), nil
	}
	h.notify()
	sb, err = h.waitSandbox(ctx, orgID, sandboxID, func(r *sandboxRow) bool { return isTerminal(r.AgentState) })
	if err != nil {
		return nil, h.internal(ctx, "delete sandbox", err)
	}
	return connect.NewResponse(&ownsandv1.DeleteSandboxResponse{Sandbox: sb.proto()}), nil
}

// pauseOrResume runs PauseSandbox (pause true) or ResumeSandbox.
func (h SandboxHandler) pauseOrResume(ctx context.Context, rawOrgID, rawSandboxID string, pause bool) (*ownsandv1.Sandbox, error) {
	orgID, err := normalizeID("organization_id", rawOrgID)
	if err != nil {
		return nil, err
	}
	if _, _, err := h.authorize(ctx, h.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	sandboxID, err := normalizeID("sandbox_id", rawSandboxID)
	if err != nil {
		return nil, err
	}
	sb, err := loadSandbox(ctx, h.DB.Pool, orgID, sandboxID, false)
	if err != nil {
		return nil, h.internal(ctx, "load sandbox", err)
	}
	from, to, rpc := ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, ownsandv1.SandboxState_SANDBOX_STATE_PAUSED, "PauseSandbox"
	info := commandInfo{kind: cmdPauseSandbox, orgID: orgID, sandboxID: sandboxID}
	cmd := &ownsandv1.AgentCommand{CommandId: newID(), Command: &ownsandv1.AgentCommand_PauseSandbox{PauseSandbox: &ownsandv1.PauseSandboxCommand{SandboxId: sandboxID}}}
	if !pause {
		from, to, rpc = to, from, "ResumeSandbox"
		info.kind = cmdResumeSandbox
		cmd.Command = &ownsandv1.AgentCommand_ResumeSandbox{ResumeSandbox: &ownsandv1.ResumeSandboxCommand{SandboxId: sandboxID}}
	}
	state, _ := sb.shownState()
	if state == to {
		return sb.proto(), nil
	}
	if state != from {
		return nil, stateErr(state, rpc)
	}
	conn := h.Hub.Live(sb.DeviceID)
	if conn == nil {
		return nil, errUnavailable("the sandbox's device has no live agent channel")
	}
	wait := h.Hub.Commands.Register(cmd.CommandId)
	defer h.Hub.Commands.Cancel(cmd.CommandId)
	if _, err := h.sendCommand(ctx, conn, info, cmd); err != nil {
		return nil, errUnavailable("the agent channel closed")
	}
	timer := time.NewTimer(h.Timing.CommandWait)
	defer timer.Stop()
	select {
	case res := <-wait:
		if e := res.GetError(); e != nil {
			code := connect.Code(e.GetCode())
			if code < connect.CodeCanceled || code > connect.CodeUnauthenticated {
				code = connect.CodeInternal
			}
			return nil, connect.NewError(code, fmt.Errorf("the agent rejected %s: %s", rpc, e.GetMessage()))
		}
	case <-timer.C:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-conn.Done():
	}
	sb, err = loadSandbox(ctx, h.DB.Pool, orgID, sandboxID, false)
	if err != nil {
		return nil, h.internal(ctx, "load sandbox", err)
	}
	return sb.proto(), nil
}

func (h SandboxHandler) PauseSandbox(ctx context.Context, req *connect.Request[ownsandv1.PauseSandboxRequest]) (*connect.Response[ownsandv1.PauseSandboxResponse], error) {
	sb, err := h.pauseOrResume(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), true)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.PauseSandboxResponse{Sandbox: sb}), nil
}

func (h SandboxHandler) ResumeSandbox(ctx context.Context, req *connect.Request[ownsandv1.ResumeSandboxRequest]) (*connect.Response[ownsandv1.ResumeSandboxResponse], error) {
	sb, err := h.pauseOrResume(ctx, req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), false)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.ResumeSandboxResponse{Sandbox: sb}), nil
}
