// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// commandKind identifies the command behind a command ID.
type commandKind int

const (
	cmdCreateSandbox commandKind = iota + 1
	cmdDeleteSandbox
	cmdPauseSandbox
	cmdResumeSandbox
	cmdCreateSnapshot
	cmdExportSnapshot
	cmdImportSnapshot
	cmdCleanUp
	cmdUpdateAgent
	cmdDeleteSnapshot
)

func (k commandKind) String() string {
	return [...]string{"", "create_sandbox", "delete_sandbox", "pause_sandbox", "resume_sandbox", "create_snapshot",
		"export_snapshot", "import_snapshot", "clean_up", "update_agent", "delete_snapshot"}[k]
}

// commandInfo is what the server remembers about a sent command, so that it
// can apply the result without a waiting RPC.
type commandInfo struct {
	kind       commandKind
	deviceID   string
	orgID      string
	sandboxID  string
	snapshotID string
}

// errDeviceRemoved closes a channel after the clean-up command.
var errDeviceRemoved = errors.New("the device was removed")

// writerDrainTimeout bounds the wait for the channel writer after the
// channel closed.
const writerDrainTimeout = 5 * time.Second

// sendCommand sends cmd on conn with a new command ID and remembers it.
func (s *Service) sendCommand(ctx context.Context, conn *hub.Conn, info commandInfo, cmd *ownsandv1.AgentCommand) (string, error) {
	if cmd.CommandId == "" {
		cmd.CommandId = newID()
	}
	info.deviceID = conn.DeviceID
	s.commands.Store(cmd.CommandId, info)
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}); err != nil {
		s.commands.Delete(cmd.CommandId)
		return "", err
	}
	s.Logger.InfoContext(ctx, "agent_command_sent", slog.String("device_id", conn.DeviceID), slog.String("command_id", cmd.CommandId),
		slog.String("command", info.kind.String()), slog.String("sandbox_id", info.sandboxID), slog.String("snapshot_id", info.snapshotID))
	return cmd.CommandId, nil
}

// sendDelete sends a delete command for a sandbox when the device has a
// live channel.
func (s *Service) sendDelete(ctx context.Context, conn *hub.Conn, orgID string, d pendingDelete) {
	_, err := s.sendCommand(ctx, conn, commandInfo{kind: cmdDeleteSandbox, orgID: orgID, sandboxID: d.sandboxID},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSandbox{DeleteSandbox: &ownsandv1.DeleteSandboxCommand{SandboxId: d.sandboxID, EndReason: d.reason}}})
	if err != nil {
		s.Logger.WarnContext(ctx, "agent_command_send_failed", slog.String("device_id", conn.DeviceID), slog.Any("error", err))
	}
}

// sendDeleteSnapshot tells a connected agent to delete its local copy of a
// snapshot whose record was deleted. The agent answers success also when
// it holds no copy and defers the deletion of a copy in use until its
// readers end; a failure (an older agent, a storage error) is only logged,
// and a later state report that lists the copy triggers the command again.
func (s *Service) sendDeleteSnapshot(ctx context.Context, conn *hub.Conn, orgID, snapshotID string) {
	_, err := s.sendCommand(ctx, conn, commandInfo{kind: cmdDeleteSnapshot, orgID: orgID, snapshotID: snapshotID},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_DeleteSnapshot{DeleteSnapshot: &ownsandv1.DeleteSnapshotCommand{SnapshotId: snapshotID}}})
	if err != nil {
		s.Logger.WarnContext(ctx, "agent_command_send_failed", slog.String("device_id", conn.DeviceID), slog.Any("error", err))
	}
}

// Channel serves the agent channel: a Connect bidirectional stream of
// AgentEnvelope messages over HTTP/2 with the `osd_` credential (recorded
// transport decision).
func (h AgentHandler) Channel(ctx context.Context, stream *connect.BidiStream[ownsandv1.ChannelRequest, ownsandv1.ChannelResponse]) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	if h.Hub == nil || h.Lease == nil {
		return errUnavailable("this instance does not accept agent channels now")
	}
	// The ownership check and the hub registration are one step under the
	// lease's ownership lock. The lease loss transition takes the same lock
	// before it closes all channels, so this channel is either in that
	// close or refused here, before the configuration is sent and before
	// the hub changes. Register is in-memory only; the lock is released
	// before any database or stream I/O below.
	var conn *hub.Conn
	if !h.Lease.WhileHeld(func() { conn = h.Hub.Register(ctx, p.ID, p.OrganizationID) }) {
		return errUnavailable("this instance does not accept agent channels now")
	}
	log := h.Logger.With(slog.String("device_id", conn.DeviceID), slog.Uint64("generation", conn.Generation))
	log.InfoContext(ctx, "agent_channel_connected")

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case env := <-conn.Outbound():
				if env == nil {
					conn.Close(errDeviceRemoved)
					return
				}
				if err := stream.Send(&ownsandv1.ChannelResponse{Envelope: env}); err != nil {
					conn.Close(err)
					return
				}
			case <-conn.Done():
				return
			}
		}
	}()
	inbound := make(chan *ownsandv1.AgentEnvelope)
	go func() {
		for {
			req, err := stream.Receive()
			if err != nil {
				conn.Close(err)
				return
			}
			select {
			case inbound <- req.GetEnvelope():
			case <-conn.Done():
				return
			}
		}
	}()

	defer func() {
		conn.Close(hub.ErrChannelClosed)
		select {
		case <-writerDone:
		case <-time.After(writerDrainTimeout):
		}
		current := h.Hub.Unregister(conn)
		cause := context.Cause(conn.Context())
		log.InfoContext(context.WithoutCancel(ctx), "agent_channel_disconnected", slog.Bool("current", current), slog.String("cause", fmt.Sprint(cause)))
		if current {
			h.markOffline(context.WithoutCancel(ctx), conn.DeviceID)
		}
	}()

	if err := h.sendConfiguration(ctx, conn); err != nil {
		return nil
	}
	timer := time.NewTimer(h.Timing.HeartbeatTimeout)
	defer timer.Stop()
	for {
		select {
		case env := <-inbound:
			if env.GetHeartbeat() != nil {
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				timer.Reset(h.Timing.HeartbeatTimeout)
			}
			h.handleEnvelope(conn.Context(), conn, env)
		case <-timer.C:
			log.InfoContext(ctx, "agent_channel_heartbeat_timeout")
			conn.Close(errors.New("no heartbeat for the heartbeat timeout"))
		case <-conn.Done():
			return nil
		}
	}
}

// markOffline sets the device offline after its current channel closed.
// Its non-terminal sandboxes then show as UNREACHABLE.
func (s *Service) markOffline(ctx context.Context, deviceID string) {
	tag, err := s.DB.Pool.Exec(ctx, "UPDATE devices SET state = $2 WHERE device_id = $1 AND state = $3",
		deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_OFFLINE), int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE))
	if err != nil {
		s.Logger.WarnContext(ctx, "device_offline_update_failed", slog.String("device_id", deviceID), slog.Any("error", err))
		return
	}
	if tag.RowsAffected() > 0 {
		s.Logger.InfoContext(ctx, "device_offline", slog.String("device_id", deviceID))
	}
}

// deviceLimits loads the limits of a device.
func (s *Service) deviceLimits(ctx context.Context, deviceID string) (*ownsandv1.DeviceLimits, error) {
	var l ownsandv1.DeviceLimits
	err := s.DB.Pool.QueryRow(ctx, "SELECT limit_vcpu_count, limit_memory_mib, limit_disk_gib, vcpu_overcommit_percent FROM devices WHERE device_id = $1", deviceID).
		Scan(&l.VcpuCount, &l.MemoryMib, &l.DiskGib, &l.VcpuOvercommitPercent)
	return &l, err
}

// sendConfiguration sends the STUN URLs and the device limits.
func (s *Service) sendConfiguration(ctx context.Context, conn *hub.Conn) error {
	limits, err := s.deviceLimits(ctx, conn.DeviceID)
	if err != nil {
		s.Logger.WarnContext(ctx, "device_limits_load_failed", slog.String("device_id", conn.DeviceID), slog.Any("error", err))
		conn.Close(err)
		return err
	}
	return conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Configuration{Configuration: &ownsandv1.AgentConfiguration{
		StunUrls: s.STUNURLs, Limits: limits,
	}}})
}

// handleEnvelope dispatches one envelope from the agent.
func (s *Service) handleEnvelope(ctx context.Context, conn *hub.Conn, env *ownsandv1.AgentEnvelope) {
	log := s.Logger.With(slog.String("device_id", conn.DeviceID))
	var err error
	switch m := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_Heartbeat:
		err = s.handleHeartbeat(ctx, conn, m.Heartbeat)
	case *ownsandv1.AgentEnvelope_CapabilityReport:
		err = s.handleCapabilityReport(ctx, conn, m.CapabilityReport)
	case *ownsandv1.AgentEnvelope_StateReport:
		err = s.handleStateReport(ctx, conn, m.StateReport)
	case *ownsandv1.AgentEnvelope_SandboxUpdate:
		err = s.handleSandboxUpdate(ctx, conn, m.SandboxUpdate.GetSandbox())
	case *ownsandv1.AgentEnvelope_CommandResult:
		s.handleCommandResult(ctx, conn, m.CommandResult)
	case *ownsandv1.AgentEnvelope_SignedUrlRequest:
		err = s.handleSignedURLRequest(ctx, conn, m.SignedUrlRequest)
	default:
		if s.DataPath != nil && s.DataPath.HandleEnvelope(ctx, conn, env) {
			return
		}
		log.InfoContext(ctx, "agent_envelope_ignored", slog.String("type", fmt.Sprintf("%T", env.GetMessage())))
	}
	if err != nil && ctx.Err() == nil {
		log.WarnContext(ctx, "agent_envelope_failed", slog.String("type", fmt.Sprintf("%T", env.GetMessage())), slog.Any("error", err))
	}
}

func (s *Service) handleHeartbeat(ctx context.Context, conn *hub.Conn, hb *ownsandv1.Heartbeat) error {
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: hb.GetSequence()}}}); err != nil {
		return err
	}
	now := s.now()
	// The first PENDING_IDLE report starts pending_idle_since, which survives
	// restarts and lease handovers; any other state clears it.
	var state int16
	// $3 is cast explicitly: Postgres deduces one type per parameter, and the
	// column assignment (smallint) and the comparison with 2 (integer) would
	// otherwise deduce two.
	err := s.DB.Pool.QueryRow(ctx, `UPDATE devices SET last_seen_time = $2, update_state = $3::smallint, update_target_version = $4,
		pending_idle_since = CASE WHEN $3::smallint = 2 THEN COALESCE(pending_idle_since, $2) ELSE NULL END
		WHERE device_id = $1 RETURNING state`,
		conn.DeviceID, now, int16(hb.GetUpdateState()), hb.GetUpdateTargetVersion()).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED) {
		conn.Close(errDeviceRemoved)
		return nil
	}
	return err
}

func (s *Service) handleCapabilityReport(ctx context.Context, conn *hub.Conn, r *ownsandv1.CapabilityReport) error {
	caps, err := proto.Marshal(r.GetCapabilities())
	if err != nil {
		return err
	}
	if _, err := s.DB.Pool.Exec(ctx, `UPDATE devices SET capabilities = $2,
		host_os = CASE WHEN $3 > 0 THEN $3 ELSE host_os END, arch = CASE WHEN $4 > 0 THEN $4 ELSE arch END,
		agent_version = CASE WHEN $5 <> '' THEN $5 ELSE agent_version END WHERE device_id = $1`,
		conn.DeviceID, caps, int16(r.GetHostOs()), int16(r.GetArch()), r.GetAgentVersion()); err != nil {
		return err
	}
	return s.evaluateUpdate(ctx, conn)
}

func (s *Service) handleStateReport(ctx context.Context, conn *hub.Conn, r *ownsandv1.StateReport) error {
	res, err := s.reconcileStateReport(ctx, conn.DeviceID, r)
	if err != nil {
		return err
	}
	ack := &ownsandv1.StateReportAck{ReportSequence: r.GetReportSequence()}
	for _, e := range r.GetSandboxes() {
		ack.Sandboxes = append(ack.Sandboxes, &ownsandv1.SandboxUpdateAck{SandboxId: e.GetSandboxId(), Revision: e.GetRevision()})
	}
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{
		Ack: &ownsandv1.StateAck_StateReport{StateReport: ack},
	}}}); err != nil {
		return err
	}
	conn.SetReconciled()
	s.Logger.InfoContext(ctx, "agent_state_report", slog.String("device_id", conn.DeviceID), slog.Int("sandboxes", len(r.GetSandboxes())),
		slog.Int("snapshots", len(r.GetSnapshots())), slog.Int("changed", len(res.changed)), slog.Int("deletes", len(res.deletes)))
	for _, id := range res.changed {
		s.watch.fire(id)
	}
	for _, d := range res.deletes {
		s.sendDelete(ctx, conn, conn.OrganizationID, d)
	}
	for _, id := range res.snapshotDeletes {
		s.sendDeleteSnapshot(ctx, conn, conn.OrganizationID, id)
	}
	return s.evaluateUpdate(ctx, conn)
}

func (s *Service) handleSandboxUpdate(ctx context.Context, conn *hub.Conn, e *ownsandv1.SandboxStateEntry) error {
	res, err := s.applySandboxUpdate(ctx, conn.DeviceID, e)
	if err != nil {
		return err
	}
	if res.applied {
		s.watch.fire(e.GetSandboxId())
	}
	// The server acknowledges every update, also a stale retry that it did
	// not apply, so the agent stops resending it.
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{
		Ack: &ownsandv1.StateAck_SandboxUpdate{SandboxUpdate: &ownsandv1.SandboxUpdateAck{SandboxId: e.GetSandboxId(), Revision: e.GetRevision()}},
	}}}); err != nil {
		return err
	}
	if res.delete != nil {
		s.sendDelete(ctx, conn, conn.OrganizationID, *res.delete)
	}
	return nil
}

// handleCommandResult applies the effect of a command result and hands it
// to a waiting RPC.
func (s *Service) handleCommandResult(ctx context.Context, conn *hub.Conn, res *ownsandv1.AgentCommandResult) {
	v, ok := s.commands.LoadAndDelete(res.GetCommandId())
	log := s.Logger.With(slog.String("device_id", conn.DeviceID), slog.String("command_id", res.GetCommandId()))
	if !ok {
		log.InfoContext(ctx, "agent_command_result_unknown")
		return
	}
	info := v.(commandInfo)
	if info.deviceID != conn.DeviceID {
		log.WarnContext(ctx, "agent_command_result_wrong_device")
		return
	}
	code := "ok"
	if res.GetError() != nil {
		code = res.GetError().GetCode().String()
	}
	log.InfoContext(ctx, "agent_command_result", slog.String("command", info.kind.String()), slog.String("result", code),
		slog.String("sandbox_id", info.sandboxID), slog.String("snapshot_id", info.snapshotID))
	var err error
	switch info.kind {
	case cmdCreateSandbox:
		if res.GetError() != nil {
			err = s.failCreateRejected(ctx, info.sandboxID)
			s.watch.fire(info.sandboxID)
		} else if e := res.GetCreateSandbox().GetSandbox(); e != nil {
			err = s.applyResultEntry(ctx, conn, e)
		}
	case cmdDeleteSandbox, cmdPauseSandbox, cmdResumeSandbox:
		if e := res.GetSandbox().GetSandbox(); res.GetError() == nil && e != nil {
			err = s.applyResultEntry(ctx, conn, e)
		}
	case cmdCreateSnapshot:
		err = s.applySnapshotResult(ctx, info, res)
	case cmdExportSnapshot:
		err = s.applyExportResult(ctx, info, res)
	case cmdImportSnapshot:
		if res.GetError() == nil {
			_, err = s.DB.Pool.Exec(ctx, "INSERT INTO snapshot_copies (snapshot_id, device_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", info.snapshotID, conn.DeviceID)
		}
	}
	if err != nil {
		log.WarnContext(ctx, "agent_command_result_apply_failed", slog.Any("error", err))
	}
	s.Hub.Commands.Deliver(res.GetCommandId(), res)
}

func (s *Service) applyResultEntry(ctx context.Context, conn *hub.Conn, e *ownsandv1.SandboxStateEntry) error {
	res, err := s.applySandboxUpdate(ctx, conn.DeviceID, e)
	if err != nil {
		return err
	}
	if res.applied {
		s.watch.fire(e.GetSandboxId())
	}
	if res.delete != nil {
		s.sendDelete(ctx, conn, conn.OrganizationID, *res.delete)
	}
	return nil
}

// releaseKey is the per-channel value of the last update target sent.
type releaseKey struct{}

// updateTarget computes the target version of a device from the
// organization policy and the agent releases table. A revoked running or
// pinned version selects the platform default as a revocation recovery,
// regardless of HOLD or PIN.
func (s *Service) updateTarget(ctx context.Context, deviceID string) (string, bool, error) {
	var orgID, running string
	var hostOS, arch int16
	if err := s.DB.Pool.QueryRow(ctx, "SELECT organization_id::text, agent_version, host_os, arch FROM devices WHERE device_id = $1", deviceID).
		Scan(&orgID, &running, &hostOS, &arch); err != nil {
		return "", false, err
	}
	policy, err := s.loadPolicy(ctx, s.DB.Pool, orgID)
	if err != nil {
		return "", false, err
	}
	var def string
	err = s.DB.Pool.QueryRow(ctx, "SELECT version FROM agent_releases WHERE host_os = $1 AND arch = $2 AND is_default", hostOS, arch).Scan(&def)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", false, err
	}
	revoked := func(v string) (bool, error) {
		var r bool
		err := s.DB.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_releases WHERE version = $1 AND host_os = $2 AND arch = $3 AND revoked)", v, hostOS, arch).Scan(&r)
		return r, err
	}
	runningRevoked, err := revoked(running)
	if err != nil {
		return "", false, err
	}
	if runningRevoked {
		return def, true, nil
	}
	switch policy.GetMode() {
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN:
		pinnedRevoked, err := revoked(policy.GetPinnedVersion())
		if err != nil {
			return "", false, err
		}
		if pinnedRevoked {
			return def, true, nil
		}
		return policy.GetPinnedVersion(), false, nil
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD:
		return "", false, nil
	}
	return def, false, nil
}

// evaluateUpdate sends an update command when the target version differs
// from the running version and from the last target sent on the channel.
func (s *Service) evaluateUpdate(ctx context.Context, conn *hub.Conn) error {
	target, recovery, err := s.updateTarget(ctx, conn.DeviceID)
	if err != nil {
		return err
	}
	var running string
	if err := s.DB.Pool.QueryRow(ctx, "SELECT agent_version FROM devices WHERE device_id = $1", conn.DeviceID).Scan(&running); err != nil {
		return err
	}
	// A revocation recovery is sent even when the same target was sent
	// before without it, because only a recovery allows a downgrade.
	sent := fmt.Sprintf("%s/%t", target, recovery)
	if target == "" || target == running || conn.Value(releaseKey{}) == sent {
		return nil
	}
	conn.SetValue(releaseKey{}, sent)
	_, err = s.sendCommand(ctx, conn, commandInfo{kind: cmdUpdateAgent, orgID: conn.OrganizationID},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_UpdateAgent{UpdateAgent: &ownsandv1.UpdateAgentCommand{TargetVersion: target, RevocationRecovery: recovery}}})
	return err
}

// versionRevoked reports whether the running version of a device is
// revoked.
func versionRevokedQuery() string {
	return "EXISTS (SELECT 1 FROM agent_releases r WHERE r.version = d.agent_version AND r.host_os = d.host_os AND r.arch = d.arch AND r.revoked)"
}
