// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/internal/semver"
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
	cmdWithdrawUpdate
)

func (k commandKind) String() string {
	return [...]string{"", "create_sandbox", "delete_sandbox", "pause_sandbox", "resume_sandbox", "create_snapshot",
		"export_snapshot", "import_snapshot", "clean_up", "update_agent", "delete_snapshot", "withdraw_agent_update"}[k]
}

// commandInfo is what the server remembers about a sent command, so that it
// can apply the result without a waiting RPC.
type commandInfo struct {
	kind     commandKind
	deviceID string
	// generation is the generation of the channel that carried the command;
	// when that channel closes, the record starts to expire.
	generation uint64
	orgID      string
	sandboxID  string
	snapshotID string
}

// errDeviceRemoved indicates that the device is removed or missing.
var errDeviceRemoved = errors.New("the device was removed")

// errInitialStateReportFailed closes a channel whose first full state report
// could not be reconciled with the database. Such a channel never becomes
// reconciled, so it is not eligible for placement, yet its heartbeats would
// keep it open until the agent restarts: the agent sends one full report per
// channel. Closing it makes the agent reconnect through its normal backoff
// and report again.
var errInitialStateReportFailed = errors.New("the first state report of the channel could not be applied")

// DefaultWriterDrainTimeout bounds the wait for the channel writer before
// the device state changes after the channel closed. A writer that is still
// blocked in a send after that (the agent does not read) no longer holds
// back the device state. The handler cannot return while the send is
// active: returning would finish the response writer under the blocked
// send, which is a data race and a panic in net/http. The handler instead
// aborts the blocked write (see abortStalledWrite) right after the drain
// timeout, before the device state update, so a delayed update does not
// keep the blocked write. It then waits for the writer to end.
const DefaultWriterDrainTimeout = 5 * time.Second

// streamControlKey is the context key of the response controller of the
// agent channel request.
type streamControlKey struct{}

// WithStreamControl wraps the handler of the agent service so that the
// channel handler can reach the response controller of its HTTP stream. The
// server sets no WriteTimeout, because the channel is a long-lived stream, so
// this controller is the only way to end a transport write that the agent
// does not drain. Connect passes the request context to the RPC handler, so
// the value is visible there.
func WithStreamControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc := http.NewResponseController(w)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), streamControlKey{}, rc)))
	})
}

// abortStalledWrite ends a transport write that blocks because the agent
// does not read its channel. conn.Close only cancels the hub context, which
// a blocked stream.Send does not observe, so without this the handler, the
// writer, the inbound reader and the full outbound queue stay until the
// agent disconnects. A write deadline in the past makes net/http reset the
// HTTP/2 stream (or fail the HTTP/1.1 connection write), and the blocked
// send returns an error. It returns an error when the abort could not be
// requested.
func abortStalledWrite(ctx context.Context) error {
	rc, ok := ctx.Value(streamControlKey{}).(*http.ResponseController)
	if !ok {
		return errors.New("the request has no response controller")
	}
	return rc.SetWriteDeadline(time.Unix(1, 0))
}

// sendCommand sends cmd on conn with a new command ID and remembers it.
func (s *Service) sendCommand(ctx context.Context, conn *hub.Conn, info commandInfo, cmd *ownsandv1.AgentCommand) (string, error) {
	if cmd.CommandId == "" {
		cmd.CommandId = newID()
	}
	info.deviceID = conn.DeviceID
	info.generation = conn.Generation
	s.commands.add(cmd.CommandId, info, conn, s.commandRetentionNow())
	if err := conn.Send(ctx, &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: cmd}}); err != nil {
		s.commands.remove(cmd.CommandId)
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

// admitChannel runs register only while the instance holds the lease and the
// control event baseline of the lease period is established, as one step
// under the ownership lock of the lease. It reports whether register ran.
func (s *Service) admitChannel(register func()) bool {
	admitted := false
	s.Lease.WhileHeld(func() {
		if s.BaselineReady != nil && !s.BaselineReady() {
			return
		}
		register()
		admitted = true
	})
	return admitted
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
	// the hub changes. RegisterAdmitted waits for the device's fence, which
	// the offline write of a closing channel of the same device holds across
	// database I/O, before it takes the ownership lock. The step under the
	// lock is in-memory only, so the lock never waits for that I/O and is
	// released before any database or stream I/O below.
	//
	// The same step also requires the control event baseline of the lease
	// period (Service.BaselineReady): a channel that observed the device
	// state or limits before the baseline could miss a control event that
	// committed between that observation and the baseline, because the
	// event processor only dispatches events after the baseline. The agent
	// reconnects through its backoff until the baseline exists.
	conn := h.Hub.RegisterAdmitted(ctx, p.ID, p.OrganizationID, h.admitChannel)
	if conn == nil {
		return errUnavailable("this instance does not accept agent channels now")
	}
	log := h.Logger.With(slog.String("device_id", conn.DeviceID), slog.Uint64("generation", conn.Generation))
	log.InfoContext(ctx, "agent_channel_connected")

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			// A closed channel wins over a queued envelope: with both ready,
			// the select below picks at random, and a writer that keeps
			// sending after the handler returned uses a finished response
			// writer (a data race, and a panic in net/http). A saturated
			// channel closes with a full queue, so this case is the normal
			// one there.
			select {
			case <-conn.Done():
				return
			default:
			}
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
		// The commands that this channel carried may still be answered on a
		// replacement channel of the device, so their records expire after
		// the grace period instead of being removed. The channel is closed
		// here, so a concurrent sendCommand either stored its record before
		// this call (and is marked) or sees the closed channel (and starts
		// the expiry itself).
		expiring := h.commands.markClosed(conn.DeviceID, conn.Generation, h.commandRetentionNow())
		stalled := false
		select {
		case <-writerDone:
		case <-time.After(h.writerDrainTimeout()):
			select {
			case <-writerDone:
			default:
				stalled = true
			}
		}
		if stalled {
			// Abort the blocked transport write before the device state
			// changes: markOffline waits on the device row lock without a
			// bound, and a blocked writer must not stay until it returns.
			// The send returns an error once the stream is reset. When the
			// abort is not possible, the wait for the writer below lasts
			// until the transport ends the stream (the agent disconnects).
			log.WarnContext(context.WithoutCancel(ctx), "agent_channel_writer_stalled", slog.String("cause", fmt.Sprint(context.Cause(conn.Context()))))
			if err := abortStalledWrite(ctx); err != nil {
				log.WarnContext(context.WithoutCancel(ctx), "agent_channel_writer_abort_failed", slog.Any("error", err))
			}
		}
		current := h.Hub.Unregister(conn)
		cause := context.Cause(conn.Context())
		log.InfoContext(context.WithoutCancel(ctx), "agent_channel_disconnected", slog.Bool("current", current), slog.String("cause", fmt.Sprint(cause)),
			slog.Int("commands_marked_for_expiry", expiring))
		if current {
			h.markOffline(context.WithoutCancel(ctx), conn.DeviceID)
		}
		if stalled {
			// The handler cannot return while the send is active: returning
			// would finish the response writer under the blocked send.
			<-writerDone
			log.InfoContext(context.WithoutCancel(ctx), "agent_channel_writer_ended")
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
			return channelReadLimitError(context.Cause(conn.Context()))
		}
	}
}

// channelReadLimitError returns the error that ends the channel handler when
// the inbound reader stopped because a message exceeded MaxRequestBytes, so
// that the agent sees resource_exhausted and not a clean end of stream. Every
// other cause ends the handler without an error, as before.
func channelReadLimitError(cause error) error {
	var ce *connect.Error
	if errors.As(cause, &ce) && ce.Code() == connect.CodeResourceExhausted {
		return ce
	}
	return nil
}

// markOffline sets the device offline after its current channel closed.
// Its non-terminal sandboxes then show as UNREACHABLE.
//
// The closing channel was current when Unregister removed it, but a
// replacement channel may have registered, and its state report may have
// committed ONLINE, before this write. The write therefore runs under the
// hub's device fence and only while the device has no registered channel:
// either the replacement registered first and the write is skipped, or the
// replacement registers after the check, and its state report cannot
// commit ONLINE before this transaction commits, because the transaction
// holds the device row lock until then. The replacement's state report
// then sets the device ONLINE after the write.
//
// Lock order: the device row lock is taken first (as the state report
// transaction does), and the fence second.
func (s *Service) markOffline(ctx context.Context, deviceID string) {
	log := s.Logger.With(slog.String("device_id", deviceID))
	var offline, ran bool
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		offline, ran = false, false
		if _, err := tx.Exec(ctx, "SELECT 1 FROM devices WHERE device_id = $1 FOR UPDATE", deviceID); err != nil {
			return err
		}
		var execErr error
		ran = s.Hub.IfNoChannel(deviceID, func() {
			var tag pgconn.CommandTag
			tag, execErr = tx.Exec(ctx, "UPDATE devices SET state = $2 WHERE device_id = $1 AND state = $3",
				deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_OFFLINE), int16(ownsandv1.DeviceState_DEVICE_STATE_ONLINE))
			offline = execErr == nil && tag.RowsAffected() > 0
		})
		return execErr
	})
	switch {
	case err != nil:
		log.WarnContext(ctx, "device_offline_update_failed", slog.Any("error", err))
	case !ran:
		log.InfoContext(ctx, "device_offline_skipped", slog.String("reason", "a newer agent channel is registered"))
	case offline:
		log.InfoContext(ctx, "device_offline")
	}
}

// deviceLimits loads the limits of a device.
func (s *Service) deviceLimits(ctx context.Context, deviceID string) (*ownsandv1.DeviceLimits, error) {
	var l ownsandv1.DeviceLimits
	err := s.DB.Pool.QueryRow(ctx, "SELECT limit_vcpu_count, limit_memory_mib, limit_disk_gib, vcpu_overcommit_percent FROM devices WHERE device_id = $1", deviceID).
		Scan(&l.VcpuCount, &l.MemoryMib, &l.DiskGib, &l.VcpuOvercommitPercent)
	return &l, err
}

// sendConfiguration sends the STUN URLs and the device limits. The channel
// handler and the control event processor both call it for one channel; the
// per-channel configuration lock makes the read of the limits and the queueing
// of the message one step, so the message that is queued last always carries
// the newest committed limits.
func (s *Service) sendConfiguration(ctx context.Context, conn *hub.Conn) error {
	defer conn.LockConfig()()
	limits, err := s.deviceLimits(ctx, conn.DeviceID)
	if hook := s.afterConfigLimitsRead.Load(); hook != nil && err == nil {
		(*hook)(conn.DeviceID)
	}
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
	res, err := s.reconcileStateReport(ctx, conn.DeviceID, conn.ID, r)
	if err != nil {
		if !conn.Reconciled() && ctx.Err() == nil {
			// No acknowledgement is sent and the channel stays not
			// reconciled. Close it so that the agent connects again and
			// sends a new full report once the database recovers. A failed
			// report on an already reconciled channel keeps the channel,
			// which is eligible and keeps its earlier state.
			s.Logger.WarnContext(ctx, "agent_state_report_failed", slog.String("device_id", conn.DeviceID),
				slog.String("channel_id", conn.ID), slog.Int("sandboxes", len(r.GetSandboxes())), slog.Any("error", err))
			conn.Close(errInitialStateReportFailed)
			return nil
		}
		return err
	}
	if res.removed {
		// The device was removed before the report took the device lock, so
		// nothing was applied. Like a heartbeat of a removed device, close the
		// channel without an acknowledgement; the agent gets the removal
		// notice at its next Authenticate.
		s.Logger.InfoContext(ctx, "agent_state_report_device_removed", slog.String("device_id", conn.DeviceID),
			slog.Int("sandboxes", len(r.GetSandboxes())), slog.Bool("removed", true))
		conn.Close(errDeviceRemoved)
		return nil
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
		slog.Int("snapshots", len(r.GetSnapshots())), slog.Int("changed", len(res.changed)), slog.Int("deletes", len(res.deletes)),
		slog.Int("recovered_copies", res.recoveredCopies))
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
		// Watchers are keyed by the lowercase canonical ID, so an agent's
		// uppercase spelling must not skip them.
		s.watch.fire(canonicalUUID(e.GetSandboxId()))
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
	info, outcome := s.commands.take(res.GetCommandId(), conn.DeviceID, s.commandRetentionNow(), s.commandGrace())
	log := s.Logger.With(slog.String("device_id", conn.DeviceID), slog.String("command_id", res.GetCommandId()), slog.String("channel_id", conn.ID))
	switch outcome {
	case takeUnknown:
		log.InfoContext(ctx, "agent_command_result_unknown")
		return
	case takeWrongDevice:
		// The record stays for the device that the command was sent to.
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
			// A transient database error is retried inside
			// Timing.CreateFailTimeout; a failure that remains closes this
			// channel while it is current, so the replacement channel's
			// state report settles the CREATING sandbox.
			var attempts int
			var channelClosed bool
			attempts, channelClosed, err = s.failUnsentCreate(ctx, conn, "create_fail_transition_retry",
				[]any{slog.String("sandbox_id", info.sandboxID), slog.String("channel_id", conn.ID)},
				func(dbCtx context.Context) error { return s.failCreateRejected(dbCtx, info.sandboxID) })
			if err != nil {
				log.ErrorContext(ctx, "create_fail_transition_failed", slog.String("sandbox_id", info.sandboxID), slog.String("reason", "agent_rejected"),
					slog.Int("attempts", attempts), slog.Bool("channel_closed", channelClosed), slog.Any("error", err))
			}
			s.watch.fire(info.sandboxID)
		} else if e := res.GetCreateSandbox().GetSandbox(); e != nil {
			err = s.applyResultEntry(ctx, conn, e)
		}
	case cmdDeleteSandbox, cmdPauseSandbox, cmdResumeSandbox:
		if e := res.GetSandbox().GetSandbox(); res.GetError() == nil && e != nil {
			err = s.applyResultEntry(ctx, conn, e)
		}
	case cmdCreateSnapshot:
		err = s.applySnapshotResult(ctx, conn, info, res)
	case cmdExportSnapshot:
		err = s.applyExportResultWithRetry(ctx, conn, info, res)
	case cmdImportSnapshot:
		if res.GetError() == nil {
			err = s.applyImportResult(ctx, conn, info, res, log)
		}
	}
	if err != nil {
		log.WarnContext(ctx, "agent_command_result_apply_failed", slog.Any("error", err))
	}
	s.Hub.Commands.Deliver(res.GetCommandId(), res)
}

// applyImportResult records the copy of a successful import result with
// applyResultWithRetry. The server removed the command record before it
// applied the result and the agent sends the result once, so a result that a
// transient database error loses would leave the copy on the device untracked
// by snapshot_copies: a later deletion of the snapshot would not include the
// device. registerImportedCopy is idempotent (the copy row is inserted with
// ON CONFLICT DO NOTHING, and a snapshot that was deleted meanwhile only
// makes the importer get the delete command), so a retry after a commit whose
// outcome is unknown changes nothing more. When the application ends without
// success while conn is still the current channel of its device, the channel
// is closed: the state report of the replacement channel then recovers the
// copy that the agent lists or removes the copy that it does not list. A
// replacement channel that already owns the device is never closed. The
// caller delivers the result to the waiting create after this returns, so the
// create observes the outcome: it sends its create command only on a channel
// that is still open.
func (s *Service) applyImportResult(ctx context.Context, conn *hub.Conn, info commandInfo, res *ownsandv1.AgentCommandResult, log *slog.Logger) error {
	return s.applyResultWithRetry(ctx, conn, resultApplyEvents{
		retry:     "import_result_apply_retry",
		recovered: "import_result_apply_recovered",
		abandoned: "import_result_apply_abandoned",
	}, errImportResultNotApplied, []any{slog.String("snapshot_id", info.snapshotID), slog.String("command_id", res.GetCommandId())},
		func(ctx context.Context) error { return s.registerImportedCopy(ctx, conn, info, log) })
}

// errImportResultNotApplied is the cause that closes a channel whose
// import-snapshot result could not be applied.
var errImportResultNotApplied = errors.New("the import-snapshot result could not be applied")

// registerImportedCopy records the local copy that a successful import
// result reports. The transaction first locks the importing device row
// FOR SHARE and skips the write when the device is absent or removed, then
// locks the parent snapshot row FOR SHARE while the copy row is inserted.
//
// The device lock keeps a copy row from outliving a removal: the removal
// holds the device row FOR UPDATE before it deletes the snapshot_copies rows
// of the device, and snapshot_copies has no foreign key on device_id, so
// without the lock an insert that commits after that delete would leave a row
// for a removed device that a later snapshot deletion would then list as a
// holder. A result that waits for an in-flight removal finds the removed
// state once the removal commits and writes nothing. No delete_snapshot is
// sent in that case: the removal already deleted the copy rows and closes the
// channel, and the agent's data for the device is the removal's concern. The
// device row comes before the snapshot row, the order of the removal (device
// FOR UPDATE, then snapshot rows FOR UPDATE), so the two cannot deadlock. The
// organization row is not locked: the device row already serializes with the
// removal.
//
// The snapshot lock conflicts with the FOR UPDATE lock that every snapshot
// deletion takes before it lists the holders of the snapshot, so the two
// orders are consistent: when the result commits first, the deletion reads
// the copy row and includes the importer in its fanout, and when the
// deletion commits first, the snapshot is absent here, no row is written and
// the importer is sent the delete command itself, because the deletion's
// fanout could not know a device that had not reported its copy yet. The
// command is sent after the transaction ended, so no database lock is held
// while the channel is written.
func (s *Service) registerImportedCopy(ctx context.Context, conn *hub.Conn, info commandInfo, log *slog.Logger) error {
	present := false
	deviceActive := true
	err := s.DB.InTx(ctx, func(tx pgx.Tx) error {
		present, deviceActive = false, true
		var state int16
		err := tx.QueryRow(ctx, "SELECT state FROM devices WHERE device_id = $1 FOR SHARE", conn.DeviceID).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			deviceActive = false
			return nil
		}
		if err != nil {
			return err
		}
		if ownsandv1.DeviceState(state) == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
			deviceActive = false
			return nil
		}
		var one int
		err = tx.QueryRow(ctx, "SELECT 1 FROM snapshots WHERE snapshot_id = $1 FOR SHARE", info.snapshotID).Scan(&one)
		if errors.Is(err, pgx.ErrNoRows) {
			present = false
			return nil
		}
		if err != nil {
			return err
		}
		present = true
		_, err = tx.Exec(ctx, "INSERT INTO snapshot_copies (snapshot_id, device_id) VALUES ($1, $2) ON CONFLICT DO NOTHING", info.snapshotID, conn.DeviceID)
		return err
	})
	if err != nil {
		return err
	}
	if !deviceActive {
		log.InfoContext(ctx, "snapshot_import_result_device_removed", slog.String("snapshot_id", info.snapshotID))
		return nil
	}
	if !present {
		log.InfoContext(ctx, "snapshot_import_result_late", slog.String("snapshot_id", info.snapshotID))
		s.sendDeleteSnapshot(ctx, conn, info.orgID, info.snapshotID)
	}
	return nil
}

func (s *Service) applyResultEntry(ctx context.Context, conn *hub.Conn, e *ownsandv1.SandboxStateEntry) error {
	res, err := s.applySandboxUpdate(ctx, conn.DeviceID, e)
	if err != nil {
		return err
	}
	if res.applied {
		// Watchers are keyed by the lowercase canonical ID, so an agent's
		// uppercase spelling must not skip them.
		s.watch.fire(canonicalUUID(e.GetSandboxId()))
	}
	if res.delete != nil {
		s.sendDelete(ctx, conn, conn.OrganizationID, *res.delete)
	}
	return nil
}

// releaseKey is the per-channel value of the last update decision queued.
type releaseKey struct{}

// withdrawKey is the releaseKey value after a withdrawal. It cannot equal a
// target key, which has the form "<version>/<recovery>".
const withdrawKey = "withdraw"

// updateDecision is what the control plane sends a device for an update.
type updateDecision struct {
	// target is the version to install; empty when none is sent.
	target string
	// recovery marks the target as a revocation recovery.
	recovery bool
	// pinned marks the target as the version of an organization PIN that is
	// not a revocation recovery. A pinned target is sent also when it equals
	// the running version, because the agent may hold a queued update that
	// the command for the running version drops.
	pinned bool
	// withdraw tells the agent to drop a queued update. It is set only under
	// HOLD when no revocation recovery applies.
	withdraw bool
}

// updateTarget computes the update decision of a device from the
// organization policy and the agent releases table. A revoked running or
// pinned version selects the platform default as a revocation recovery,
// regardless of HOLD or PIN. A PIN sends the pinned version even when it is
// lower than or equal to the running version. Under FOLLOW_DEFAULT the
// platform default is a target only when it is strictly newer than the
// running version, so a default that was rolled back never downgrades a
// healthy device. HOLD
// sends no target and asks the agent to withdraw an update that it queued
// earlier.
func (s *Service) updateTarget(ctx context.Context, deviceID string) (updateDecision, error) {
	var orgID, running string
	var hostOS, arch int16
	err := s.DB.Pool.QueryRow(ctx, "SELECT organization_id::text, agent_version, host_os, arch FROM devices WHERE device_id = $1 AND state <> $2",
		deviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)).Scan(&orgID, &running, &hostOS, &arch)
	if errors.Is(err, pgx.ErrNoRows) {
		return updateDecision{}, errDeviceRemoved
	}
	if err != nil {
		return updateDecision{}, err
	}
	policy, err := s.loadPolicy(ctx, s.DB.Pool, orgID)
	if err != nil {
		return updateDecision{}, err
	}
	var def string
	err = s.DB.Pool.QueryRow(ctx, "SELECT version FROM agent_releases WHERE host_os = $1 AND arch = $2 AND is_default", hostOS, arch).Scan(&def)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return updateDecision{}, err
	}
	revoked := func(v string) (bool, error) {
		var r bool
		err := s.DB.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM agent_releases WHERE version = $1 AND host_os = $2 AND arch = $3 AND revoked)", v, hostOS, arch).Scan(&r)
		return r, err
	}
	runningRevoked, err := revoked(running)
	if err != nil {
		return updateDecision{}, err
	}
	if runningRevoked {
		return updateDecision{target: def, recovery: true}, nil
	}
	switch policy.GetMode() {
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_PIN:
		pinnedRevoked, err := revoked(policy.GetPinnedVersion())
		if err != nil {
			return updateDecision{}, err
		}
		if pinnedRevoked {
			return updateDecision{target: def, recovery: true}, nil
		}
		return updateDecision{target: policy.GetPinnedVersion(), pinned: true}, nil
	case ownsandv1.AgentVersionPolicyMode_AGENT_VERSION_POLICY_MODE_HOLD:
		return updateDecision{withdraw: true}, nil
	}
	if def == "" {
		return updateDecision{}, nil
	}
	cmp, err := semver.CompareStrings(def, running)
	if err != nil {
		// Without an order the server cannot tell an upgrade from a
		// downgrade, so it sends no ordinary default target.
		s.Logger.WarnContext(ctx, "agent_update_skipped", slog.String("device_id", deviceID),
			slog.String("reason", "version_not_comparable"), slog.String("default_version", def),
			slog.String("running_version", running), slog.Any("error", err))
		return updateDecision{}, nil
	}
	if cmp <= 0 {
		return updateDecision{}, nil
	}
	return updateDecision{target: def}, nil
}

// evaluateUpdate sends an update command when the target version differs
// from the last target sent on the channel and, except for a pinned target,
// from the running version. A pin that equals the running version is sent
// once per channel and decision, because the agent may hold a queued newer
// update that the command for its running version drops. The evaluation
// also sends the withdrawal of a queued update when the policy is HOLD and no
// revocation recovery applies, once per channel and policy state: also after
// reconciliation on a new channel, because the agent may still hold an
// update that an earlier channel queued, and again after a target was sent in
// between, so a change back to FOLLOW_DEFAULT resends the target.
//
// Mixed versions: an agent that predates the withdrawal command answers it
// with UNIMPLEMENTED. The server only logs that result (agent_command_result)
// and does not retry or negotiate; such an agent keeps its queued update.
//
// The channel handler (capability and state reports) and the control event
// processor (policy changes) both call it for the same channel, so the whole
// decision, from reading the policy to queueing the command, runs under the
// per-channel update lock. A decision that read an older policy then cannot
// send its command after the command of a newer decision.
func (s *Service) evaluateUpdate(ctx context.Context, conn *hub.Conn) error {
	defer conn.LockUpdates()()
	if conn.Context().Err() != nil {
		return nil
	}
	d, err := s.updateTarget(ctx, conn.DeviceID)
	if errors.Is(err, errDeviceRemoved) {
		return nil
	}
	if err != nil {
		return err
	}
	lastDecision := conn.Value(releaseKey{})
	if d.withdraw {
		if lastDecision == withdrawKey {
			return nil
		}
		_, err = s.sendCommand(ctx, conn, commandInfo{kind: cmdWithdrawUpdate, orgID: conn.OrganizationID},
			&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_WithdrawAgentUpdate{WithdrawAgentUpdate: &ownsandv1.WithdrawAgentUpdateCommand{}}})
		if err == nil {
			conn.SetValue(releaseKey{}, withdrawKey)
		}
		return err
	}
	var running string
	err = s.DB.Pool.QueryRow(ctx, "SELECT agent_version FROM devices WHERE device_id = $1 AND state <> $2",
		conn.DeviceID, int16(ownsandv1.DeviceState_DEVICE_STATE_REMOVED)).Scan(&running)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	// A revocation recovery is sent even when the same target was sent
	// before without it, because only a recovery allows a downgrade.
	sent := fmt.Sprintf("%s/%t", d.target, d.recovery)
	if d.target == "" || (d.target == running && !d.pinned) || lastDecision == sent {
		// A completed decision can end the HOLD period even without a
		// target. The next HOLD then sends its own withdrawal.
		if lastDecision == withdrawKey {
			conn.SetValue(releaseKey{}, nil)
		}
		return nil
	}
	_, err = s.sendCommand(ctx, conn, commandInfo{kind: cmdUpdateAgent, orgID: conn.OrganizationID},
		&ownsandv1.AgentCommand{Command: &ownsandv1.AgentCommand_UpdateAgent{UpdateAgent: &ownsandv1.UpdateAgentCommand{TargetVersion: d.target, RevocationRecovery: d.recovery}}})
	if err == nil {
		conn.SetValue(releaseKey{}, sent)
	}
	return err
}

// versionRevoked reports whether the running version of a device is
// revoked.
func versionRevokedQuery() string {
	return "EXISTS (SELECT 1 FROM agent_releases r WHERE r.version = d.agent_version AND r.host_os = d.host_os AND r.arch = d.arch AND r.revoked)"
}
