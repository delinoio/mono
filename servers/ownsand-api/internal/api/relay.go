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

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

// RelayGrantHeader carries the grant ID of a RelayService request.
const RelayGrantHeader = "Ownsand-Relay-Grant-Id"

// RelayAgentHandler serves RelayAgentService.
type RelayAgentHandler struct{ *DataPlane }

var _ ownsandv1connect.RelayAgentServiceHandler = RelayAgentHandler{}

// Connect registers the agent relay connection of the credential's device,
// bound to its current agent channel generation, and serves it until it
// closes, a newer connection replaces it or that agent channel closes.
func (h RelayAgentHandler) Connect(ctx context.Context, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
	p, err := principal(ctx)
	if err != nil {
		return err
	}
	hc := h.S.Hub.Get(p.ID)
	if hc == nil {
		return errUnavailable("the device has no agent channel on this instance")
	}
	conn := relay.NewConn(ctx, p.ID, hc.Generation, h.Limits)
	h.Relays.Register(conn)
	defer h.Relays.Unregister(conn)
	log := h.S.Logger.With(slog.String("device_id", p.ID), slog.Uint64("generation", hc.Generation))
	log.InfoContext(ctx, "relay_connection_opened")
	go func() {
		select {
		case <-hc.Done():
			conn.Close(errors.New("the agent channel closed"))
		case <-conn.Done():
		}
	}()
	err = conn.Run(func() (*ownsandv1.DataFrame, error) {
		req, err := stream.Receive()
		if err != nil {
			return nil, err
		}
		return req.GetFrame(), nil
	}, func(f *ownsandv1.DataFrame) error {
		return stream.Send(&ownsandv1.ConnectResponse{Frame: f})
	})
	log.InfoContext(context.WithoutCancel(ctx), "relay_connection_closed", slog.String("cause", fmt.Sprint(err)), slog.Int("open_streams", conn.StreamCount()))
	return nil
}

// RelayHandler serves RelayService.
type RelayHandler struct{ *DataPlane }

var _ ownsandv1connect.RelayServiceHandler = RelayHandler{}

// errGrantInvalid is unavailable with GRANT_INVALID in the detail.
func errGrantInvalid() error {
	return errorFromDetail(&ownsandv1.ErrorDetail{
		Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "the relay grant is unknown, expired or revoked",
		SessionEndReason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID,
	})
}

// relayCall is one relay RPC.
type relayCall struct {
	procedure string
	grant     *grants.Grant
	stream    *relay.Stream
	conn      *relay.Conn
	start     time.Time
	frames    int
	bytes     int64
}

// begin authenticates a relay RPC: it checks the grant in the header, the
// principal, the authorization matrix and the request fields against the
// grant, waits up to 10 seconds for the agent relay connection and opens a
// relay stream.
func (p *DataPlane) begin(ctx context.Context, procedure string, header http.Header, orgID, sandboxID string, kind ownsandv1.RelayOperationKind, opID string) (*relayCall, error) {
	pr, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	g := p.Grants.Admittable(header.Get(RelayGrantHeader))
	if g == nil {
		key := fmt.Sprintf("relay:%d/%s", pr.Kind, pr.ID)
		if ok, retry := p.failures.Allow(key, 1); !ok {
			return nil, errRateLimited(retry)
		}
		return nil, errGrantInvalid()
	}
	if g.Principal != pr {
		return nil, errPermission("the relay grant belongs to another principal")
	}
	if _, _, err := p.S.authorize(ctx, p.S.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	kindOK := g.Kind == kind || (kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC && g.Kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC)
	if g.OrgID != orgID || g.SandboxID != sandboxID || !kindOK || g.OperationID != opID {
		return nil, errPermission("the request does not match the relay grant")
	}
	hc := p.S.Hub.Get(g.DeviceID)
	if hc == nil || hc.Generation != g.ChannelGen {
		return nil, errGrantInvalid()
	}
	conn := p.Relays.Wait(ctx, g.DeviceID, g.ChannelGen, relayWaitForConn)
	if conn == nil {
		return nil, errUnavailable("the device has no relay connection")
	}
	s, err := conn.Open(ctx, g.ID, g.SandboxID)
	if errors.Is(err, relay.ErrTooManyStreams) {
		return nil, errRateLimited(time.Second)
	}
	if err != nil {
		return nil, errUnavailable("the relay connection closed")
	}
	return &relayCall{procedure: procedure, grant: g, stream: s, conn: conn, start: time.Now()}, nil
}

func (p *DataPlane) logCall(ctx context.Context, c *relayCall, err error) {
	p.S.Logger.LogAttrs(ctx, slog.LevelInfo, "relay_rpc",
		slog.String("procedure", c.procedure), slog.String("grant_id", c.grant.ID),
		slog.String("principal_kind", c.grant.Principal.Kind.String()), slog.String("principal_id", c.grant.Principal.ID),
		slog.String("organization_id", c.grant.OrgID), slog.String("sandbox_id", c.grant.SandboxID), slog.String("device_id", c.grant.DeviceID),
		slog.Uint64("stream_id", c.stream.ID), slog.String("code", codeString(err)), slog.Int("frames", c.frames), slog.Int64("bytes", c.bytes),
		slog.Int64("duration_ms", time.Since(c.start).Milliseconds()))
}

// streamEvents is how a server-streaming relay RPC turns frames into
// responses.
type streamEvents struct {
	// payload converts an agent payload frame into a response and reports
	// whether it ends the RPC; ok is false for a frame of another kind.
	payload   func(f *ownsandv1.DataFrame) (resp any, terminal bool, ok bool)
	accepted  func() error
	keepalive func() error
	send      func(resp any) error
}

// sendRequest forwards the request frame within the agent's credit and the
// request deadline.
func (p *DataPlane) sendRequest(ctx context.Context, c *relayCall, f *ownsandv1.DataFrame) error {
	deadline := c.stream.Started.Add(p.Timing.RequestDeadline)
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if err := c.stream.Send(rctx, f); err != nil {
		if errors.Is(err, relay.ErrConnClosed) {
			return errUnavailable("the relay connection closed")
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the request was not forwarded within the request deadline"))
	}
	return nil
}

// endFromAgent maps an agent session-end or error frame to the RPC error.
func endFromAgent(f *ownsandv1.DataFrame) (error, bool) {
	switch pl := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_SessionEnd:
		return errorFromDetail(&ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, Message: "the agent ended the relay stream",
			SessionEndReason: pl.SessionEnd.GetReason()}), true
	case *ownsandv1.DataFrame_Error:
		return errorFromDetail(pl.Error), true
	}
	return nil, false
}

// serveStream runs a server-streaming relay RPC after begin: it forwards the
// request, sends keepalives at least every keepalive interval (also before
// the accepted event), ends a stream without the agent's accepted notice
// after the acceptance timeout (restarted whenever the connection receives
// a frame, because a notice cannot overtake a large frame in transit), and
// rotates an accepted stream with an OK status before 14 minutes.
func (p *DataPlane) serveStream(ctx context.Context, c *relayCall, request *ownsandv1.DataFrame, ev streamEvents) (err error) {
	sendClose := true
	defer func() {
		c.stream.Close(sendClose)
		p.logCall(ctx, c, err)
	}()
	// Keepalives also cover the request upload and the wait for credit.
	uploadDone := make(chan error, 1)
	go func() { uploadDone <- p.sendRequest(ctx, c, request) }()
	tick := min(p.Timing.Keepalive, p.Timing.Acceptance) / 4
	ticker := time.NewTicker(tick)
	defer ticker.Stop()
	lastEmit := time.Now()
	accepted := false
	forwarded := false
	var acceptBy time.Time
	rotateAt := c.stream.Started.Add(p.Timing.Rotation)
	for {
		if !forwarded {
			select {
			case err := <-uploadDone:
				if err != nil {
					return err
				}
				forwarded = true
				acceptBy = time.Now().Add(p.Timing.Acceptance)
			default:
			}
		}
		f, nerr := c.stream.Next(ctx, ticker.C)
		if nerr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if errors.Is(nerr, relay.ErrUnknownKind) {
				return connect.NewError(connect.CodeInternal, nerr)
			}
			return errUnavailable("the relay connection closed")
		}
		now := time.Now()
		if f == nil {
			if accepted && !now.Before(rotateAt) {
				return nil // rotation: OK status without a final event
			}
			if forwarded && !accepted {
				if last := c.conn.LastReceive().Add(p.Timing.Acceptance); last.After(acceptBy) {
					acceptBy = last
				}
				if !now.Before(acceptBy) {
					return errUnavailable("the agent did not accept the request")
				}
			}
			if now.Sub(lastEmit) >= p.Timing.Keepalive {
				if err := ev.keepalive(); err != nil {
					return err
				}
				lastEmit = now
			}
			continue
		}
		if e, ok := endFromAgent(f); ok {
			sendClose = false
			return e
		}
		if f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
			if !accepted {
				accepted = true
				if err := ev.accepted(); err != nil {
					return err
				}
				lastEmit = now
			}
			continue
		}
		resp, terminal, ok := ev.payload(f)
		if !ok {
			return connect.NewError(connect.CodeInternal, errors.New("unexpected frame from the agent"))
		}
		if err := ev.send(resp); err != nil {
			return err
		}
		c.frames++
		c.bytes += datapath.FrameCost(f)
		lastEmit = now
		c.stream.Consumed(ctx, f)
		if terminal {
			return nil
		}
	}
}

func isTerminalExecEvent(e *ownsandv1.ExecEvent) bool {
	switch e.GetEvent().(type) {
	case *ownsandv1.ExecEvent_Exited, *ownsandv1.ExecEvent_Failed, *ownsandv1.ExecEvent_SandboxEnded, *ownsandv1.ExecEvent_Paused:
		return true
	}
	return false
}

func execEvents(send func(*ownsandv1.ExecEvent) error) streamEvents {
	return streamEvents{
		payload: func(f *ownsandv1.DataFrame) (any, bool, bool) {
			e := f.GetExecEvent()
			if e == nil {
				return nil, false, false
			}
			return e, isTerminalExecEvent(e), true
		},
		accepted: func() error {
			return send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}})
		},
		keepalive: func() error {
			return send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}})
		},
		send: func(r any) error { return send(r.(*ownsandv1.ExecEvent)) },
	}
}

func (h RelayHandler) Exec(ctx context.Context, req *connect.Request[ownsandv1.ExecRequest], stream *connect.ServerStream[ownsandv1.ExecResponse]) error {
	m := req.Msg
	if err := validate.ExecRequest(m); err != nil {
		return errInvalid(err)
	}
	c, err := h.begin(ctx, ownsandv1connect.RelayServiceExecProcedure, req.Header(), m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, m.GetExecId())
	if err != nil {
		return err
	}
	return h.serveStream(ctx, c, datapath.MustNewFrame(0, "", m), execEvents(func(e *ownsandv1.ExecEvent) error {
		return stream.Send(&ownsandv1.ExecResponse{Event: e})
	}))
}

func (h RelayHandler) AttachExec(ctx context.Context, req *connect.Request[ownsandv1.AttachExecRequest], stream *connect.ServerStream[ownsandv1.AttachExecResponse]) error {
	m := req.Msg
	if err := validate.AttachExecRequest(m); err != nil {
		return errInvalid(err)
	}
	c, err := h.begin(ctx, ownsandv1connect.RelayServiceAttachExecProcedure, req.Header(), m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, m.GetExecId())
	if err != nil {
		return err
	}
	return h.serveStream(ctx, c, datapath.MustNewFrame(0, "", m), execEvents(func(e *ownsandv1.ExecEvent) error {
		return stream.Send(&ownsandv1.AttachExecResponse{Event: e})
	}))
}

func (h RelayHandler) ReadFile(ctx context.Context, req *connect.Request[ownsandv1.ReadFileRequest], stream *connect.ServerStream[ownsandv1.ReadFileResponse]) error {
	m := req.Msg
	if err := validate.ReadFileRequest(m); err != nil {
		return errInvalid(err)
	}
	c, err := h.begin(ctx, ownsandv1connect.RelayServiceReadFileProcedure, req.Header(), m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, m.GetReadId())
	if err != nil {
		return err
	}
	send := func(e *ownsandv1.FileReadEvent) error { return stream.Send(&ownsandv1.ReadFileResponse{Event: e}) }
	return h.serveStream(ctx, c, datapath.MustNewFrame(0, "", m), streamEvents{
		payload: func(f *ownsandv1.DataFrame) (any, bool, bool) {
			e := f.GetFileReadEvent()
			if e == nil {
				return nil, false, false
			}
			return e, e.GetEnd() != nil, true
		},
		accepted: func() error {
			return send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}})
		},
		keepalive: func() error {
			return send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}})
		},
		send: func(r any) error { return send(r.(*ownsandv1.FileReadEvent)) },
	})
}

func (h RelayHandler) WriteFile(ctx context.Context, req *connect.Request[ownsandv1.WriteFileRequest]) (resp *connect.Response[ownsandv1.WriteFileResponse], err error) {
	m := req.Msg
	if err := validate.WriteFileRequest(m, false); err != nil {
		return nil, errInvalid(err)
	}
	c, err := h.begin(ctx, ownsandv1connect.RelayServiceWriteFileProcedure, req.Header(), m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, m.GetWriteId())
	if err != nil {
		return nil, err
	}
	sendClose := true
	defer func() {
		c.stream.Close(sendClose)
		h.logCall(ctx, c, err)
	}()
	// The whole call, including the upload and the agent's result, must
	// finish within the request deadline.
	cctx, cancel := context.WithDeadline(ctx, c.stream.Started.Add(h.Timing.RequestDeadline))
	defer cancel()
	if err := h.sendRequest(cctx, c, datapath.MustNewFrame(0, "", m)); err != nil {
		return nil, err
	}
	for {
		f, nerr := c.stream.Next(cctx, nil)
		if nerr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if cctx.Err() != nil {
				return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("the write did not finish within the call deadline"))
			}
			return nil, errUnavailable("the relay connection closed")
		}
		if e, ok := endFromAgent(f); ok {
			sendClose = false
			return nil, e
		}
		if r := f.GetFileWriteResult(); r != nil {
			c.frames++
			c.bytes += datapath.FrameCost(f)
			c.stream.Consumed(ctx, f)
			return connect.NewResponse(r), nil
		}
	}
}
