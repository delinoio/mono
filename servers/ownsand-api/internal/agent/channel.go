// SPDX-License-Identifier: FSL-1.1-ALv2

package agent

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/rpc"
	"github.com/delinoio/mono/servers/ownsand-api/internal/store"
)

// Serve runs one agent channel. Message order when the channel opens: the
// server sends the configuration message at once; the agent sends a
// capability report (optional) and then its full state report. The device
// becomes online, and eligible for scheduling, only after the server applied
// that state report. Heartbeats are accepted (and acknowledged) from the
// first message on.
func (h *Hub) Serve(ctx context.Context, stream *connect.BidiStream[ownsandv1.AgentEnvelope, ownsandv1.AgentEnvelope]) error {
	p, ok := auth.FromContext(ctx)
	if !ok || p.Kind != ownsandv1.PrincipalKind_PRINCIPAL_KIND_DEVICE {
		return auth.ErrMissingCredential
	}
	rpc.SetLogOrganization(ctx, p.OrganizationID)
	if !h.Ready() {
		return ErrNoLease
	}
	d, err := h.Store.GetDeviceByID(ctx, p.ID)
	if err != nil || d.State == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
		return connect.NewError(connect.CodePermissionDenied, errors.New("the device was removed"))
	}
	c, old := h.register(d.ID, d.OrganizationID)
	if old != nil {
		old.Close("replaced by a new channel")
	}
	start := time.Now()
	h.Logger.Info("agent_channel_connected", slog.String("device_id", c.DeviceID), slog.String("organization_id", c.OrganizationID), slog.Uint64("generation", c.Generation))
	// The configuration message goes first; it also flushes the response
	// headers, so the agent knows at once that the channel was accepted.
	h.SendConfiguration(ctx, c)

	recvErr := make(chan error, 1)
	heartbeat := make(chan struct{}, 1)
	go func() {
		for {
			env, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			if env.GetHeartbeat() != nil {
				select {
				case heartbeat <- struct{}{}:
				default:
				}
			}
			h.handle(ctx, c, env)
		}
	}()

	timeout := h.HeartbeatTimeout
	if timeout == 0 {
		timeout = HeartbeatTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var result error
loop:
	for {
		select {
		case env := <-c.out:
			if err := stream.Send(env); err != nil {
				c.Close("send failed")
				break loop
			}
		case <-heartbeat:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(timeout)
		case <-timer.C:
			c.Close("heartbeat timeout")
			break loop
		case err := <-recvErr:
			c.Close("agent closed the channel")
			if err != nil && !errors.Is(err, context.Canceled) && connect.CodeOf(err) != connect.CodeCanceled && !isEOF(err) {
				h.Logger.Debug("agent_channel_receive_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
			}
			break loop
		case <-c.done:
			result = connect.NewError(connect.CodeUnavailable, errors.New("agent channel closed by the server"))
			break loop
		case <-ctx.Done():
			c.Close("request ended")
			break loop
		}
	}
	h.closed(c, time.Since(start))
	return result
}

func isEOF(err error) bool { return err != nil && err.Error() == "EOF" }

// closed runs once per channel after it closed.
func (h *Hub) closed(c *Conn, d time.Duration) {
	c.Close("closed")
	current := h.unregister(c)
	reason, _ := c.reason.Load().(string)
	h.Logger.Info("agent_channel_disconnected", slog.String("device_id", c.DeviceID), slog.Uint64("generation", c.Generation),
		slog.String("reason", reason), slog.Bool("current", current), slog.Duration("duration", d))
	if current && h.Lease != nil && h.Lease.Held() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.Store.SetDeviceOffline(ctx, c.DeviceID); err != nil {
			h.Logger.Warn("device_offline_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		} else {
			h.Logger.Info("device_offline", slog.String("device_id", c.DeviceID), slog.String("reason", reason))
		}
		h.notifyChange()
	}
	if h.Extension != nil {
		h.Extension.ChannelClosed(c)
	}
}

func (h *Hub) handle(ctx context.Context, c *Conn, env *ownsandv1.AgentEnvelope) {
	switch m := env.GetMessage().(type) {
	case *ownsandv1.AgentEnvelope_Heartbeat:
		h.onHeartbeat(ctx, c, m.Heartbeat)
	case *ownsandv1.AgentEnvelope_CapabilityReport:
		if err := h.Store.UpdateCapabilities(ctx, c.DeviceID, m.CapabilityReport); err != nil {
			h.Logger.Warn("capability_report_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		}
	case *ownsandv1.AgentEnvelope_StateReport:
		h.onStateReport(ctx, c, m.StateReport)
	case *ownsandv1.AgentEnvelope_SandboxUpdate:
		h.onSandboxUpdate(ctx, c, m.SandboxUpdate.GetSandbox())
	case *ownsandv1.AgentEnvelope_CommandResult:
		h.onCommandResult(ctx, c, m.CommandResult)
	case *ownsandv1.AgentEnvelope_SnapshotUpdate:
		u := m.SnapshotUpdate
		if err := h.Store.ApplySnapshotReport(ctx, c.DeviceID, u.GetSnapshot(), u.GetExportState(), u.GetPartSha256()); err != nil {
			h.Logger.Warn("snapshot_update_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		}
	case *ownsandv1.AgentEnvelope_UpdateStatus:
		if err := h.Store.UpdateStatus(ctx, c.DeviceID, m.UpdateStatus.GetAgentVersion(), m.UpdateStatus.GetUpdateState()); err != nil {
			h.Logger.Warn("update_status_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		}
	case *ownsandv1.AgentEnvelope_SignedUrlRequest:
		h.onSignedURLRequest(ctx, c, m.SignedUrlRequest)
	default:
		if h.Extension != nil && h.Extension.HandleEnvelope(c, env) {
			return
		}
		h.Logger.Debug("agent_message_ignored", slog.String("device_id", c.DeviceID))
	}
}

func (h *Hub) onHeartbeat(ctx context.Context, c *Conn, hb *ownsandv1.Heartbeat) {
	if err := h.Store.TouchDevice(ctx, c.DeviceID); err != nil {
		h.Logger.Warn("heartbeat_store_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
	}
	// Check the device state at each heartbeat and close the channel of a
	// removed device after its clean-up command.
	if d, err := h.Store.GetDeviceByID(ctx, c.DeviceID); err == nil && d.State == ownsandv1.DeviceState_DEVICE_STATE_REMOVED {
		h.cleanUpAndClose(c)
		return
	} else if errors.Is(err, store.ErrNotFound) {
		h.cleanUpAndClose(c)
		return
	}
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: hb.GetSequence()}}})
}

// cleanUpAndClose sends the clean-up command of a removed device and then
// closes its channel.
func (h *Hub) cleanUpAndClose(c *Conn) {
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_Command{Command: &ownsandv1.AgentCommand{
		CommandId: store.NewID(), Command: &ownsandv1.AgentCommand_CleanUp{CleanUp: &ownsandv1.CleanUpCommand{}},
	}}})
	go func() {
		// Give the writer a moment to flush the clean-up command.
		time.Sleep(200 * time.Millisecond)
		c.Close("device removed")
	}()
}

func (h *Hub) onStateReport(ctx context.Context, c *Conn, rep *ownsandv1.StateReport) {
	out, err := h.Store.ReconcileStateReport(ctx, c.DeviceID, rep)
	if errors.Is(err, store.ErrConflict) {
		h.cleanUpAndClose(c)
		return
	}
	if err != nil {
		h.Logger.Warn("state_report_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		return
	}
	c.online.Store(true)
	h.Logger.Info("state_report_applied", slog.String("device_id", c.DeviceID), slog.Int("sandboxes", len(rep.GetSandboxes())),
		slog.Int("snapshots", len(rep.GetSnapshots())), slog.Int("ended", len(out.Ended)), slog.Int("deletes", len(out.Deletes)))
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{Target: &ownsandv1.StateAck_ReportId{ReportId: rep.GetReportId()}}}})
	for id, reason := range out.Deletes {
		h.SendDelete(c.DeviceID, id, reason)
	}
	h.notifyChange()
}

func (h *Hub) onSandboxUpdate(ctx context.Context, c *Conn, r *ownsandv1.SandboxReport) {
	out, err := h.Store.ApplySandboxReport(ctx, c.DeviceID, r)
	if err != nil {
		h.Logger.Warn("sandbox_update_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		return
	}
	if out.Ack {
		c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_StateAck{StateAck: &ownsandv1.StateAck{Target: &ownsandv1.StateAck_Sandbox{
			Sandbox: &ownsandv1.SandboxAck{SandboxId: r.GetSandboxId(), Revision: r.GetRevision()},
		}}}})
	}
	if out.SendDelete {
		h.SendDelete(c.DeviceID, r.GetSandboxId(), out.DeleteReason)
	}
	if out.Changed {
		h.notifyChange()
	}
}

func (h *Hub) onCommandResult(ctx context.Context, c *Conn, res *ownsandv1.AgentCommandResult) {
	h.mu.Lock()
	w := h.waiters[res.GetCommandId()]
	delete(h.waiters, res.GetCommandId())
	h.mu.Unlock()
	h.Logger.Info("agent_command_result", slog.String("device_id", c.DeviceID), slog.String("command_id", res.GetCommandId()), slog.String("error_code", res.GetErrorCode()))
	if w != nil && w.deviceID == c.DeviceID {
		switch {
		case w.kind == commandCreateSandbox && res.GetErrorCode() != "":
			// The agent rejected the create before it made a VM.
			if err := h.Store.FailCreate(ctx, c.DeviceID, w.sandboxID); err != nil {
				h.Logger.Warn("create_failed_store_failed", slog.String("sandbox_id", w.sandboxID), slog.Any("error", err))
			}
		case w.kind == commandCreateSnapshot && res.GetErrorCode() != "":
			if err := h.Store.ApplySnapshotReport(ctx, c.DeviceID, &ownsandv1.SnapshotReport{SnapshotId: w.snapshotID, State: ownsandv1.SnapshotState_SNAPSHOT_STATE_FAILED}, 0, nil); err != nil {
				h.Logger.Warn("snapshot_failed_store_failed", slog.String("snapshot_id", w.snapshotID), slog.Any("error", err))
			}
		case w.kind == commandExportSnapshot && res.GetErrorCode() != "":
			if err := h.Store.ApplySnapshotReport(ctx, c.DeviceID, &ownsandv1.SnapshotReport{SnapshotId: w.snapshotID}, ownsandv1.SnapshotExportState_SNAPSHOT_EXPORT_STATE_EXPORT_FAILED, nil); err != nil {
				h.Logger.Warn("export_failed_store_failed", slog.String("snapshot_id", w.snapshotID), slog.Any("error", err))
			}
		}
	}
	if sb := res.GetSandbox(); sb != nil {
		h.onSandboxUpdate(ctx, c, sb)
	}
	if sn := res.GetSnapshot(); sn != nil {
		if err := h.Store.ApplySnapshotReport(ctx, c.DeviceID, sn, 0, nil); err != nil {
			h.Logger.Warn("snapshot_result_failed", slog.String("device_id", c.DeviceID), slog.Any("error", err))
		}
	}
	if w != nil {
		w.ch <- res
	}
	h.notifyChange()
}

func (h *Hub) onSignedURLRequest(ctx context.Context, c *Conn, req *ownsandv1.SignedUrlRequest) {
	var grant *ownsandv1.SignedUrlGrant
	if h.SignedURLs == nil {
		grant = &ownsandv1.SignedUrlGrant{RequestId: req.GetRequestId(), SnapshotId: req.GetSnapshotId(), ErrorCode: connect.CodeUnavailable.String()}
	} else {
		grant = h.SignedURLs.Grant(ctx, c.DeviceID, req)
	}
	// Never log the URLs.
	h.Logger.Info("signed_url_request", slog.String("device_id", c.DeviceID), slog.String("snapshot_id", req.GetSnapshotId()),
		slog.Bool("upload", req.GetUpload()), slog.Int("parts", len(req.GetPartNumbers())), slog.String("error_code", grant.GetErrorCode()))
	c.Send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignedUrlGrant{SignedUrlGrant: grant}})
}
