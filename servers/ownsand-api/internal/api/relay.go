// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/validate"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
	"github.com/delinoio/mono/servers/ownsand-api/internal/config"
	"github.com/delinoio/mono/servers/ownsand-api/internal/grants"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

// RelayGrantHeader carries the grant ID of a RelayService request.
const RelayGrantHeader = "Ownsand-Relay-Grant-Id"

// MaxRequestBytes is the largest request message that any handler reads, in
// bytes after decompression. connect-go reads a request message before the
// interceptor chain runs and, without a limit, buffers and inflates it
// without bound, so the server sets the limit on every handler. It applies to
// each message of a stream, not to the stream as a whole.
//
// The value is 2 MiB. The largest legitimate message is a relay WriteFile
// request that carries 1 MiB of file data: protojson encodes the bytes field
// as base64, which is about 1.4 MB, and the binary codec needs 1 MiB plus the
// identifiers. The largest relay frame, datapath.MaxFrameSize (1 MiB plus
// 64 KiB, 1,179,648 bytes), travels inside a RelayAgentService.Connect
// message, and a full agent state report is far smaller than both. 2 MiB
// leaves headroom above all of them.
const MaxRequestBytes = 2 << 20

// Compile-time check that the limit holds the largest relay frame with room
// to spare; it fails to build when MaxFrameSize grows past the limit.
const _ = uint(MaxRequestBytes - datapath.MaxFrameSize - 1)

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
	log := h.S.Logger.With(slog.String("device_id", p.ID), slog.Uint64("generation", hc.Generation))
	// The channel lookup above and the registration are separate steps; a
	// reconnect can install a newer channel and its relay connection between
	// them, so the registry rechecks that hc is still current under its lock.
	err = h.Relays.Register(conn, func() bool {
		cur := h.S.Hub.Get(p.ID)
		return cur != nil && cur.Generation == hc.Generation
	})
	if err != nil {
		conn.Close(err)
		log.WarnContext(ctx, "relay_connection_rejected_stale_channel")
		return errUnavailable("the agent channel of this device is no longer current")
	}
	defer h.Relays.Unregister(conn)
	log.InfoContext(ctx, "relay_connection_opened")
	go func() {
		select {
		case <-hc.Done():
			conn.Close(errors.New("the agent channel closed"))
		case <-conn.Done():
		}
	}()
	err = serveRelayConn(ctx, conn, stream)
	log.InfoContext(context.WithoutCancel(ctx), "relay_connection_closed", slog.String("cause", fmt.Sprint(err)), slog.Int("open_streams", conn.StreamCount()))
	return nil
}

// relayAgentControllerKey carries the response controller of an agent relay
// request.
type relayAgentControllerKey struct{}

// RelayAgentWriteInterrupt makes the response controller of each request
// available to RelayAgentHandler.Connect. A Connect handler sees no response
// writer, but only the controller can make a blocked response write fail:
// returning from the handler does not cancel a write that waits for the
// peer's flow control, and the HTTP/2 server would finish the response while
// that write still uses it. Wrap the RelayAgentService handler with it. It
// also counts the bytes written to the response, which the relay egress of
// WriteFile request frames is measured with.
func RelayAgentWriteInterrupt(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), relayAgentControllerKey{}, http.NewResponseController(w))
		written := &atomic.Int64{}
		ctx = context.WithValue(ctx, agentWireBytesKey{}, written)
		next.ServeHTTP(&agentWireWriter{ResponseWriter: w, n: written}, r.WithContext(ctx))
	})
}

// relaySendInterrupt returns the function that makes a blocked response write
// of the request fail, or nil when the handler was not wrapped with
// RelayAgentWriteInterrupt (the relay then bounds its wait for the writer).
func relaySendInterrupt(ctx context.Context) func() error {
	rc, _ := ctx.Value(relayAgentControllerKey{}).(*http.ResponseController)
	if rc == nil {
		return nil
	}
	// A deadline in the past fails the pending and every later write.
	return func() error { return rc.SetWriteDeadline(time.Now()) }
}

// serveRelayConn runs conn over the Connect stream until it closes. It
// returns only after the last stream.Send ended, so the handler never returns
// while a send still writes to the response.
func serveRelayConn(ctx context.Context, conn *relay.Conn, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
	conn.SetSendInterrupt(relaySendInterrupt(ctx))
	conn.SetWireBytes(agentWireBytes(ctx))
	return conn.Run(func() (*ownsandv1.DataFrame, error) {
		req, err := stream.Receive()
		if err != nil {
			return nil, err
		}
		return req.GetFrame(), nil
	}, func(f *ownsandv1.DataFrame) error {
		return stream.Send(&ownsandv1.ConnectResponse{Frame: f})
	})
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

// relayGate is the state that the relay request gate hands to the handler of
// the same RPC: the call that the gate opened before Connect read the request
// body and the absolute deadline of the whole call.
type relayGate struct {
	call *relayCall
	// deadline is the request deadline counted from the stream_id allocation.
	// It bounds the request upload, the forwarding and, for WriteFile, the
	// agent's result.
	deadline time.Time
	// clearRead lifts the body read deadline once Connect has decoded the
	// request, so that it does not end a stream that outlives the upload.
	clearRead func()
	// claimed is set by the handler, which then owns the call, or by the
	// release of a call whose handler never ran.
	claimed atomic.Bool
}

type (
	relayGateKey       struct{}
	relayControllerKey struct{}
)

func relayGateFrom(ctx context.Context) *relayGate {
	g, _ := ctx.Value(relayGateKey{}).(*relayGate)
	return g
}

// RelayReadDeadlines makes the response controller of each request available
// to the relay request gate. A Connect gate sees no response writer, but only
// the controller can interrupt a request body read that blocks: net/http does
// not cancel a body read when the request context ends. Wrap the RelayService
// handler with it.
func RelayReadDeadlines(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := context.WithValue(r.Context(), relayControllerKey{}, http.NewResponseController(w))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// RelayGate returns the request gate of the RelayService handler. Before
// Connect reads the request body, it authenticates the caller, checks the
// grant in the request headers, waits for the agent relay connection and
// opens the relay stream, so that the request deadline starts at the stream
// allocation and covers the upload of the body: it arms a read deadline on
// the request body at that deadline, and an upload that exceeds it ends with
// deadline_exceeded. A call whose handler never runs, because the body failed
// to arrive or to decode, closes its stream when the RPC ends. The handler
// takes the call with begin.
//
// The checks that need request fields (organization, sandbox, kind and
// operation ID against the grant) stay in begin, after Connect decoded the
// request.
func (p *DataPlane) RelayGate(authn *Authenticator, mode config.ClientAddressMode) connect.RequestGateFunc {
	logs := &logInterceptor{logger: p.S.Logger, now: p.S.Now, addressMode: mode}
	auths := &authInterceptor{authn: authn}
	return func(ctx context.Context, spec connect.Spec, peer connect.Peer, header http.Header) (context.Context, error) {
		start := p.S.Now()
		ctx, info := logs.start(ctx, peer, header)
		ctx, err := auths.authenticate(ctx, spec.Procedure, header)
		var c *relayCall
		if err == nil {
			c, err = p.open(ctx, spec.Procedure, header)
		}
		if err != nil {
			// A rejected RPC never reaches the interceptor chain, so the gate
			// writes its RPC log record.
			logs.log(ctx, spec.Procedure, info, start, err)
			return nil, err
		}
		g := &relayGate{call: c, deadline: c.stream.Started.Add(p.Timing.RequestDeadline), clearRead: func() {}}
		if rc, ok := ctx.Value(relayControllerKey{}).(*http.ResponseController); ok {
			if err := rc.SetReadDeadline(g.deadline); err != nil {
				// Without the controller the request body read is unbounded,
				// as before the gate existed; the forwarding deadline still holds.
				p.S.Logger.WarnContext(ctx, "relay_body_deadline_unsupported", slog.String("procedure", spec.Procedure), slog.String("error", err.Error()))
			} else {
				g.clearRead = func() { _ = rc.SetReadDeadline(time.Time{}) }
			}
		}
		// The context of the RPC ends when its handler returns, or when the
		// request ends before the handler ran.
		context.AfterFunc(ctx, func() { p.releaseGate(ctx, g, spec.Procedure, info, logs, start) })
		return context.WithValue(ctx, relayGateKey{}, g), nil
	}
}

// releaseGate closes the stream of a call whose handler never took it
// (begin): the request body did not arrive within the deadline, the client
// went away or the body did not decode. The agent sees a stream close and no
// request payload.
func (p *DataPlane) releaseGate(ctx context.Context, g *relayGate, procedure string, info *rpcInfo, logs *logInterceptor, start time.Time) {
	if g.claimed.Swap(true) {
		return
	}
	// The RPC has ended, so its response writer must not be used any more
	// (net/http reuses it); the read deadline ends with the request.
	g.call.stream.Close(true)
	// The gate does not see why Connect rejected the request; a deadline that
	// has passed is the cause that the contract names, anything else is a
	// request that ended before its handler ran.
	err := error(connect.NewError(connect.CodeCanceled, errors.New("the request ended before the relay handler ran")))
	if !time.Now().Before(g.deadline) {
		err = connect.NewError(connect.CodeDeadlineExceeded, errors.New("the request body was not received within the request deadline"))
	}
	ctx = context.WithoutCancel(ctx)
	p.logCall(ctx, g.call, err)
	info.mu.Lock()
	entered := info.entered
	info.mu.Unlock()
	if !entered {
		logs.log(ctx, procedure, info, start, err)
	}
}

// rejectGrant charges one token of the caller's failed grant check bucket for
// a relay RPC that the grant check rejected with rejection (a missing or
// non-admittable grant, a grant of another principal or a request that does
// not match the grant). It returns rejection while the bucket has tokens and
// a resource_exhausted error with the retry delay when it is empty. A valid
// grant never charges the bucket.
func (p *DataPlane) rejectGrant(pr auth.Principal, rejection error) error {
	key := fmt.Sprintf("relay:%d/%s", pr.Kind, pr.ID)
	if ok, retry := p.failures.Allow(key, 1); !ok {
		return errRateLimited(retry)
	}
	return rejection
}

// open checks the relay grant in the request headers for the authenticated
// caller, waits up to 10 seconds for the agent relay connection and opens a
// relay stream.
func (p *DataPlane) open(ctx context.Context, procedure string, header http.Header) (*relayCall, error) {
	pr, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	g := p.Grants.Admittable(header.Get(RelayGrantHeader))
	if g == nil {
		return nil, p.rejectGrant(pr, errGrantInvalid())
	}
	if g.Principal != pr {
		return nil, p.rejectGrant(pr, errPermission("the relay grant belongs to another principal"))
	}
	// The organization of the request is not known yet; begin compares it
	// with the grant's after Connect decoded the request.
	if _, _, err := p.S.authorize(ctx, p.S.DB.Pool, g.OrgID, authz.ActionUseSandboxes); err != nil {
		return nil, err
	}
	// Exec, AttachExec, ReadFile and WriteFile are rejected while the
	// grant's organization is suspended; a stream that is already open is
	// not ended.
	if err := p.S.checkBilling(ctx, p.S.DB.Pool, g.OrgID, restrictWhenSuspended); err != nil {
		return nil, err
	}
	hc := p.S.Hub.Get(g.DeviceID)
	if hc == nil || hc.Generation != g.ChannelGen {
		return nil, errGrantInvalid()
	}
	conn := p.Relays.Wait(ctx, g.DeviceID, g.ChannelGen, p.Timing.connWait())
	if conn == nil {
		// Wait returns nil when the connection wait expired and when the RPC
		// context ended first. Only the second case is the caller's deadline
		// or cancellation; an expired connection wait stays unavailable.
		if err := admissionContextError(ctx); err != nil {
			return nil, err
		}
		return nil, errUnavailable("the device has no relay connection")
	}
	s, err := conn.Open(ctx, g.ID, g.SandboxID)
	if errors.Is(err, relay.ErrTooManyStreams) {
		return nil, errRateLimited(time.Second)
	}
	if err != nil {
		// Open returns the context error while it waits for its turn or for
		// room in the outbound queue; a closed connection stays unavailable.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			if cerr := admissionContextError(ctx); cerr != nil {
				return nil, cerr
			}
		}
		return nil, errUnavailable("the relay connection closed")
	}
	return &relayCall{procedure: procedure, grant: g, stream: s, conn: conn, start: time.Now()}, nil
}

// admissionContextError maps the end of the RPC context during relay
// admission: deadline_exceeded for a deadline and canceled for a
// cancellation. It returns nil while the context is active.
func admissionContextError(ctx context.Context) error {
	switch ctx.Err() {
	case nil:
		return nil
	case context.DeadlineExceeded:
		return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the request deadline passed before the relay stream opened"))
	default:
		return connect.NewError(connect.CodeCanceled, errors.New("the request was canceled before the relay stream opened"))
	}
}

// begin takes over the call that the request gate opened for a relay RPC,
// lifts the body read deadline, and checks the decoded request: validation
// (verr) and its organization, sandbox, kind and operation ID against the
// grant. It returns the call and the absolute deadline of the whole call. A
// request that fails a check closes the stream with a stream close frame.
//
// A handler that runs without the gate (a handler served directly) opens the
// call here and gets the same deadline from the stream allocation.
func (p *DataPlane) begin(ctx context.Context, procedure string, header http.Header, verr error, orgID, sandboxID string, kind ownsandv1.RelayOperationKind, opID string) (*relayCall, time.Time, error) {
	g := relayGateFrom(ctx)
	if g == nil {
		c, err := p.open(ctx, procedure, header)
		if err != nil {
			return nil, time.Time{}, err
		}
		g = &relayGate{call: c, deadline: c.stream.Started.Add(p.Timing.RequestDeadline), clearRead: func() {}}
		g.claimed.Store(true)
	} else if !g.claimed.CompareAndSwap(false, true) {
		return nil, time.Time{}, connect.NewError(connect.CodeInternal, errors.New("the relay call is already taken"))
	}
	g.clearRead()
	c := g.call
	err := verr
	if err != nil {
		err = errInvalid(err)
	} else {
		err = p.matchGrant(ctx, c.grant, orgID, sandboxID, kind, opID)
	}
	if err != nil {
		c.stream.Close(true)
		p.logCall(ctx, c, err)
		return nil, time.Time{}, err
	}
	return c, g.deadline, nil
}

// matchGrant checks the fields of the decoded request against the grant.
func (p *DataPlane) matchGrant(ctx context.Context, g *grants.Grant, orgID, sandboxID string, kind ownsandv1.RelayOperationKind, opID string) error {
	if g.OrgID != orgID {
		// The gate authorized the grant's organization. Authorize the
		// requested one too, so that a caller who is no member of it gets the
		// authorization error and not the grant mismatch.
		if _, _, err := p.S.authorize(ctx, p.S.DB.Pool, orgID, authz.ActionUseSandboxes); err != nil {
			return err
		}
	}
	kindOK := g.Kind == kind || (kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC && g.Kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC)
	if g.OrgID != orgID || g.SandboxID != sandboxID || !kindOK || g.OperationID != opID {
		return p.rejectGrant(g.Principal, errPermission("the request does not match the relay grant"))
	}
	return nil
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
// request deadline, which the request gate counted from the stream_id
// allocation, so the upload of the request body already used part of it.
// onWritten, when set, receives the bytes of the frame on the agent
// connection once the relay wrote it. When confirmed is set, sendRequest
// returns only after the connection writer finished sending the frame, so
// the request deadline also covers the wait behind a blocked writer and the
// caller can start a timer for the agent's reply at the return; otherwise it
// returns when the frame is queued.
func (p *DataPlane) sendRequest(ctx context.Context, c *relayCall, f *ownsandv1.DataFrame, deadline time.Time, onWritten func(int64), confirmed bool) error {
	rctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	send := c.stream.SendMetered
	if confirmed {
		send = c.stream.SendConfirmed
	}
	if err := send(rctx, f, onWritten); err != nil {
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
// request within the deadline of the call, sends keepalives at least every keepalive interval (also before
// the accepted event), ends a stream without the agent's accepted notice
// after the acceptance timeout (restarted when the connection receives a
// large frame, because a notice cannot overtake a large frame in transit), and
// rotates an accepted stream with an OK status before 14 minutes.
func (p *DataPlane) serveStream(ctx context.Context, c *relayCall, deadline time.Time, request *ownsandv1.DataFrame, ev streamEvents) (err error) {
	sendClose := true
	defer func() {
		c.stream.Close(sendClose)
		p.logCall(ctx, c, err)
	}()
	// Keepalives also cover the request upload and the wait for credit. The
	// request counts as forwarded only when the connection writer sent it, so
	// the acceptance timeout below never runs while the frame still waits
	// behind a blocked writer; that wait stays under the request deadline.
	uploadDone := make(chan error, 1)
	go func() { uploadDone <- p.sendRequest(ctx, c, request, deadline, nil, true) }()
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
				// The relay already ended the stream with an error frame.
				sendClose = false
				return connect.NewError(connect.CodeUnimplemented, nerr)
			}
			return errUnavailable("the relay connection closed")
		}
		now := time.Now()
		// The deadline holds on every successful receive, not only on an empty
		// wake: Next returns queued frames before it looks at the wake channel,
		// so a continuously populated queue would otherwise never rotate. The
		// frame received at or after the deadline is not forwarded; the caller
		// resumes from its last delivered offsets.
		if accepted && !now.Before(rotateAt) {
			return nil // rotation: OK status without a final event
		}
		if f == nil {
			if forwarded && !accepted {
				// Only a large frame extends the wait: the notice cannot overtake
				// it. Keepalives and small frames never do, so they cannot keep
				// a stalled admission alive.
				if last := c.conn.LastLargeReceive(); !last.IsZero() {
					if until := last.Add(p.Timing.Acceptance); until.After(acceptBy) {
						acceptBy = until
					}
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

// canonicalizeIDs rewrites validated UUID fields of a relay request to their
// lowercase text form in place. The grant check and the agent compare the
// organization, sandbox and operation identifiers as text, and the request
// frame that is forwarded to the agent must spell them like the grant does.
// exec_id is not listed: it is lowercase-only and validation rejects any other
// spelling before this runs.
func canonicalizeIDs(ids ...*string) {
	for _, id := range ids {
		*id = canonicalID(*id)
	}
}

func (h RelayHandler) Exec(ctx context.Context, req *connect.Request[ownsandv1.ExecRequest], stream *connect.ServerStream[ownsandv1.ExecResponse]) error {
	m := req.Msg
	verr := validate.ExecRequest(m)
	if verr == nil {
		canonicalizeIDs(&m.OrganizationId, &m.SandboxId)
	}
	c, deadline, err := h.begin(ctx, ownsandv1connect.RelayServiceExecProcedure, req.Header(), verr, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, m.GetExecId())
	if err != nil {
		return err
	}
	startRelayEgress(ctx, c.grant.OrgID)
	return h.serveStream(ctx, c, deadline, datapath.MustNewFrame(0, "", m), execEvents(func(e *ownsandv1.ExecEvent) error {
		return stream.Send(&ownsandv1.ExecResponse{Event: e})
	}))
}

func (h RelayHandler) AttachExec(ctx context.Context, req *connect.Request[ownsandv1.AttachExecRequest], stream *connect.ServerStream[ownsandv1.AttachExecResponse]) error {
	m := req.Msg
	verr := validate.AttachExecRequest(m)
	if verr == nil {
		canonicalizeIDs(&m.OrganizationId, &m.SandboxId)
	}
	c, deadline, err := h.begin(ctx, ownsandv1connect.RelayServiceAttachExecProcedure, req.Header(), verr, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, m.GetExecId())
	if err != nil {
		return err
	}
	startRelayEgress(ctx, c.grant.OrgID)
	return h.serveStream(ctx, c, deadline, datapath.MustNewFrame(0, "", m), execEvents(func(e *ownsandv1.ExecEvent) error {
		return stream.Send(&ownsandv1.AttachExecResponse{Event: e})
	}))
}

func (h RelayHandler) ReadFile(ctx context.Context, req *connect.Request[ownsandv1.ReadFileRequest], stream *connect.ServerStream[ownsandv1.ReadFileResponse]) error {
	m := req.Msg
	verr := validate.ReadFileRequest(m)
	if verr == nil {
		canonicalizeIDs(&m.OrganizationId, &m.SandboxId, &m.ReadId)
	}
	c, deadline, err := h.begin(ctx, ownsandv1connect.RelayServiceReadFileProcedure, req.Header(), verr, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE, m.GetReadId())
	if err != nil {
		return err
	}
	startRelayEgress(ctx, c.grant.OrgID)
	send := func(e *ownsandv1.FileReadEvent) error { return stream.Send(&ownsandv1.ReadFileResponse{Event: e}) }
	return h.serveStream(ctx, c, deadline, datapath.MustNewFrame(0, "", m), streamEvents{
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
	verr := validate.WriteFileRequest(m, false)
	if verr == nil {
		canonicalizeIDs(&m.OrganizationId, &m.SandboxId, &m.WriteId)
	}
	c, deadline, err := h.begin(ctx, ownsandv1connect.RelayServiceWriteFileProcedure, req.Header(), verr, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, m.GetWriteId())
	if err != nil {
		return nil, err
	}
	sendClose := true
	defer func() {
		c.stream.Close(sendClose)
		h.logCall(ctx, c, err)
	}()
	// The whole call, including the upload and the agent's result, must
	// finish within the request deadline of the call.
	cctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	// Relay egress toward the agent: the WriteFile request frame leaves the
	// Droplet on the agent's relay connection.
	orgID := c.grant.OrgID
	egress := func(n int64) { h.S.recordUsage(orgID, ownsandv1.BillingMeter_BILLING_METER_RELAY_EGRESS, n) }
	if err := h.sendRequest(cctx, c, datapath.MustNewFrame(0, "", m), deadline, egress, false); err != nil {
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
			if errors.Is(nerr, relay.ErrUnknownKind) {
				// The relay already ended the stream with an error frame.
				sendClose = false
				return nil, connect.NewError(connect.CodeUnimplemented, nerr)
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
			// Returning the credit of the result waits for room in the shared
			// outbound queue of the agent connection, which a stalled agent can
			// keep full. Bound that wait by the call deadline too, so a
			// pending credit return cannot hold the call (and the stream's
			// reserved close frame, which is queued only after this call
			// returns) beyond the request deadline. The result counts as
			// received either way: its frame and bytes are accounted above.
			c.stream.Consumed(cctx, f)
			if ctx.Err() != nil {
				// The caller left first and keeps its precedence, as when
				// Next fails.
				return nil, ctx.Err()
			}
			if cctx.Err() != nil {
				return nil, connect.NewError(connect.CodeDeadlineExceeded, errors.New("the write did not finish within the call deadline"))
			}
			return connect.NewResponse(r), nil
		}
	}
}
