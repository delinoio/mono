// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/auth"
	"github.com/delinoio/mono/servers/ownsand-api/internal/authz"
)

// relayStream is one relay RPC on an agent relay connection. window, credit
// and queue are guarded by conn.mu.
type relayStream struct {
	id      uint64
	sandbox string
	g       *grant
	conn    *agentConn
	window  int64 // bytes the agent may still send toward the caller
	credit  int64 // bytes the control plane may still send toward the agent
	queue   []*ownsandv1.DataFrame
	sig     chan struct{}

	failOnce sync.Once
	failCh   chan struct{}
	failErr  error

	framesIn, framesOut int
	bytesIn, bytesOut   int64
}

func (st *relayStream) notify() {
	select {
	case st.sig <- struct{}{}:
	default:
	}
}

func (st *relayStream) fail(err error) {
	st.failOnce.Do(func() {
		st.failErr = err
		close(st.failCh)
	})
}

func (st *relayStream) pop() *ownsandv1.DataFrame {
	st.conn.mu.Lock()
	defer st.conn.mu.Unlock()
	if len(st.queue) == 0 {
		return nil
	}
	f := st.queue[0]
	st.queue = st.queue[1:]
	return f
}

// closeStream ends the stream from the control plane side with a relay
// stream close frame. The stream ID is never reused on the connection.
func (st *relayStream) closeStream() {
	c := st.conn
	c.mu.Lock()
	_, open := c.streams[st.id]
	delete(c.streams, st.id)
	c.mu.Unlock()
	if open {
		c.send(datapath.NewFrame(st.id, st.sandbox, func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_RelayStreamClose{RelayStreamClose: &ownsandv1.RelayStreamClose{}}
		}))
	}
}

// forget removes a stream that the agent ended.
func (st *relayStream) forget() {
	st.conn.mu.Lock()
	delete(st.conn.streams, st.id)
	st.conn.mu.Unlock()
}

// sendRequest forwards a request payload frame within the agent's credit,
// waiting until deadline for the credit.
func (st *relayStream) sendRequest(ctx context.Context, f *ownsandv1.DataFrame, deadline time.Time) error {
	size := datapath.FrameSize(f)
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	for {
		st.conn.mu.Lock()
		// A session-end or error frame may arrive before any credit.
		if len(st.queue) > 0 {
			k := datapath.KindOf(st.queue[0])
			if k == ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END || k == ownsandv1.DataFrameKind_DATA_FRAME_KIND_ERROR {
				st.conn.mu.Unlock()
				return errFrameFirst
			}
		}
		if st.credit >= size {
			st.credit -= size
			st.conn.mu.Unlock()
			if !st.conn.send(f) {
				return connect.NewError(connect.CodeUnavailable, errors.New("the agent relay connection closed"))
			}
			st.framesOut++
			st.bytesOut += size
			return nil
		}
		st.conn.mu.Unlock()
		select {
		case <-st.sig:
		case <-st.failCh:
			return st.failErr
		case <-timer.C:
			return connect.NewError(connect.CodeDeadlineExceeded, errors.New("the request was not forwarded in time"))
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

var errFrameFirst = errors.New("agent ended the stream before the request")

// RelayService implements ownsandv1connect.RelayServiceHandler.
type RelayService struct{ M *Manager }

var _ ownsandv1connect.RelayServiceHandler = RelayService{}

type rpcInfo struct {
	procedure string
	org       string
	sandbox   string
	opID      string
	kinds     []ownsandv1.RelayOperationKind // grant kinds that allow the RPC
}

// begin authenticates the relay RPC against its grant, waits for the agent
// relay connection, opens the relay stream and forwards the request frame.
func (m *Manager) begin(ctx context.Context, header http.Header, info rpcInfo, request func(f *ownsandv1.DataFrame)) (*relayStream, time.Time, error) {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil, time.Time{}, auth.ErrMissingCredential
	}
	failKey := "relay:" + principalKey(p)
	if !m.failRate.Peek(failKey) {
		return nil, time.Time{}, connect.NewError(connect.CodeResourceExhausted, errors.New("too many failed relay calls; retry later"))
	}
	fail := func(err error) (*relayStream, time.Time, error) {
		m.failRate.Allow(failKey)
		return nil, time.Time{}, err
	}
	grantID := header.Get(datapath.RelayGrantHeader)
	now := m.now()
	m.mu.Lock()
	g := m.grants[grantID]
	if g == nil || !m.admittableAt(g, now) {
		m.mu.Unlock()
		return fail(errGrantInvalid())
	}
	if !g.principal.Same(p) || g.org != info.org || g.sandbox != info.sandbox || g.opID != info.opID || !slices.Contains(info.kinds, g.kind) {
		m.mu.Unlock()
		// The request body arrives before the handler runs (Connect decodes
		// it), so a mismatched request is rejected before any relay stream
		// opens and needs no stream close frame.
		return fail(connect.NewError(connect.CodePermissionDenied, errors.New("the request does not match the relay grant")))
	}
	if g.streams >= m.Limits.StreamsPerGrant {
		m.mu.Unlock()
		return nil, time.Time{}, retryError("too many open relay streams for this grant", m.Limits.RetryDelay)
	}
	g.streams++
	m.mu.Unlock()
	release := func() {
		m.mu.Lock()
		g.streams--
		m.mu.Unlock()
	}
	if _, err := m.API.Authorize(ctx, info.org, authz.ActionUseSandboxes); err != nil {
		release()
		return fail(err)
	}
	conn, err := m.waitConn(ctx, g.device, g.gen)
	if err != nil {
		release()
		return nil, time.Time{}, err
	}
	conn.mu.Lock()
	if int64(len(conn.streams)+1)*m.Limits.StreamBuffer > m.Limits.ConnectionBuffer {
		conn.mu.Unlock()
		release()
		return nil, time.Time{}, retryError("the device's relay connection is at its stream limit", m.Limits.RetryDelay)
	}
	conn.nextID++
	st := &relayStream{id: conn.nextID, sandbox: info.sandbox, g: g, conn: conn, window: m.Limits.StreamBuffer, sig: make(chan struct{}, 1), failCh: make(chan struct{})}
	conn.streams[st.id] = st
	conn.mu.Unlock()
	alloc := time.Now()
	conn.send(datapath.NewFrame(st.id, st.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayStreamOpen{RelayStreamOpen: &ownsandv1.RelayStreamOpen{GrantId: g.id, InitialWindow: m.Limits.StreamBuffer}}
	}))
	req := datapath.NewFrame(st.id, st.sandbox, request)
	if err := st.sendRequest(ctx, req, alloc.Add(m.Limits.RequestDeadline)); err != nil {
		if errors.Is(err, errFrameFirst) {
			// Let the pump report the agent's session-end or error frame.
			return st, alloc, nil
		}
		st.closeStream()
		m.endRPC(st)
		return nil, time.Time{}, err
	}
	return st, alloc, nil
}

func (m *Manager) endRPC(st *relayStream) {
	m.mu.Lock()
	st.g.streams--
	m.mu.Unlock()
}

// agentFrameError turns an agent session-end or error frame into the RPC
// error, or returns nil.
func agentFrameError(f *ownsandv1.DataFrame) error {
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_SessionEnd:
		return sessionEndError(p.SessionEnd.GetReason(), "the device agent ended the relay stream")
	case *ownsandv1.DataFrame_Error:
		var code connect.Code
		if code.UnmarshalText([]byte(p.Error.GetCode())) != nil {
			code = connect.CodeUnknown
		}
		return connect.NewError(code, errors.New("the device agent rejected the request"))
	}
	return nil
}

type emitter struct {
	accepted  func() error
	keepalive func() error
	// payload sends one agent payload frame and reports whether it was final.
	payload func(f *ownsandv1.DataFrame) (bool, error)
}

// pump forwards the agent's frames of a server-streaming RPC to the caller
// until a final frame, an agent end, a rotation or a failure.
func (m *Manager) pump(ctx context.Context, st *relayStream, alloc time.Time, rc *http.ResponseController, e emitter) error {
	l := m.Limits
	tickEvery := min(l.AcceptTimeout, l.StreamKeepalive, time.Second) / 4
	tick := time.NewTicker(max(tickEvery, 5*time.Millisecond))
	defer tick.Stop()
	acceptBy := time.Now().Add(l.AcceptTimeout)
	accepted := false
	lastEmit := time.Now()
	write := func(fn func() error) error {
		if rc != nil {
			_ = rc.SetWriteDeadline(time.Now().Add(l.StalledReader))
			defer func() { _ = rc.SetWriteDeadline(time.Time{}) }()
		}
		err := fn()
		lastEmit = time.Now()
		return err
	}
	for {
		for f := st.pop(); f != nil; f = st.pop() {
			if err := agentFrameError(f); err != nil {
				st.forget()
				return err
			}
			if _, ok := f.GetPayload().(*ownsandv1.DataFrame_RequestAccepted); ok {
				accepted = true
				if err := write(e.accepted); err != nil {
					st.closeStream()
					return err
				}
				continue
			}
			size := datapath.FrameSize(f)
			st.framesIn++
			st.bytesIn += size
			final, err := false, error(nil)
			if err = write(func() error { var e2 error; final, e2 = e.payload(f); return e2 }); err != nil {
				st.closeStream()
				return err
			}
			if datapath.PayloadUsesCredit(f) {
				st.conn.mu.Lock()
				st.window += size
				st.conn.mu.Unlock()
				st.conn.send(datapath.NewFrame(st.id, st.sandbox, func(x *ownsandv1.DataFrame) {
					x.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: size}}
				}))
			}
			if final {
				st.closeStream()
				return nil
			}
		}
		select {
		case <-st.sig:
		case <-st.failCh:
			return st.failErr
		case <-ctx.Done():
			st.closeStream()
			return ctx.Err()
		case <-tick.C:
			now := time.Now()
			if !accepted && now.After(acceptBy) {
				if st.conn.tracker.inTransit() {
					// A frame from the agent is still arriving; the accepted
					// notice cannot overtake it.
					acceptBy = now.Add(tickEvery)
				} else {
					st.closeStream()
					return connect.NewError(connect.CodeUnavailable, errors.New("the device agent did not accept the request in time"))
				}
			}
			if accepted && now.Sub(alloc) >= l.StreamRotation {
				// Stream rotation: OK without a final event.
				st.closeStream()
				return nil
			}
			if now.Sub(lastEmit) >= l.StreamKeepalive {
				if err := write(e.keepalive); err != nil {
					st.closeStream()
					return err
				}
			}
		}
	}
}

func (m *Manager) logRPC(ctx context.Context, procedure string, st *relayStream, info rpcInfo, start time.Time, err error) {
	p, _ := auth.FromContext(ctx)
	code := "ok"
	if err != nil {
		code = connect.CodeOf(err).String()
	}
	attrs := []slog.Attr{
		slog.String("procedure", procedure), slog.String("principal_kind", p.Kind.String()), slog.String("principal_id", p.ID),
		slog.String("organization_id", info.org), slog.String("sandbox_id", info.sandbox), slog.String("code", code), slog.Duration("duration", time.Since(start)),
	}
	if st != nil {
		attrs = append(attrs, slog.String("grant_id", st.g.id), slog.String("device_id", st.g.device),
			slog.Int("frames_in", st.framesIn), slog.Int("frames_out", st.framesOut), slog.Int64("bytes_in", st.bytesIn), slog.Int64("bytes_out", st.bytesOut))
	}
	m.Logger.LogAttrs(ctx, slog.LevelInfo, "relay_rpc", attrs...)
}

func controller(ctx context.Context) *http.ResponseController {
	rc, _ := ctx.Value(controllerKey{}).(*http.ResponseController)
	return rc
}

var execKinds = []ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC}
var attachKinds = []ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC}

func execEmitter(stream *connect.ServerStream[ownsandv1.ExecEvent]) emitter {
	return emitter{
		accepted: func() error {
			return stream.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Accepted_{Accepted: &ownsandv1.ExecEvent_Accepted{}}})
		},
		keepalive: func() error {
			return stream.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive_{Keepalive: &ownsandv1.ExecEvent_Keepalive{}}})
		},
		payload: func(f *ownsandv1.DataFrame) (bool, error) {
			ev := f.GetExecEvent()
			if ev == nil {
				return false, nil
			}
			final := ev.GetExited() != nil || ev.GetFailed() != nil || ev.GetSandboxEnded() != nil || ev.GetPaused() != nil
			return final, stream.Send(ev)
		},
	}
}

func (r RelayService) serverStream(ctx context.Context, header http.Header, info rpcInfo, request func(*ownsandv1.DataFrame), e emitter) error {
	m := r.M
	start := time.Now()
	st, alloc, err := m.begin(ctx, header, info, request)
	if err == nil {
		err = m.pump(ctx, st, alloc, controller(ctx), e)
		m.endRPC(st)
	}
	m.logRPC(ctx, info.procedure, st, info, start, err)
	return err
}

func (r RelayService) Exec(ctx context.Context, req *connect.Request[ownsandv1.ExecRequest], stream *connect.ServerStream[ownsandv1.ExecEvent]) error {
	msg := req.Msg
	info := rpcInfo{procedure: ownsandv1connect.RelayServiceExecProcedure, org: msg.GetOrganizationId(), sandbox: msg.GetSandboxId(), opID: msg.GetExecId(), kinds: execKinds}
	return r.serverStream(ctx, req.Header(), info, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecRequest{ExecRequest: msg}
	}, execEmitter(stream))
}

func (r RelayService) AttachExec(ctx context.Context, req *connect.Request[ownsandv1.AttachExecRequest], stream *connect.ServerStream[ownsandv1.ExecEvent]) error {
	msg := req.Msg
	info := rpcInfo{procedure: ownsandv1connect.RelayServiceAttachExecProcedure, org: msg.GetOrganizationId(), sandbox: msg.GetSandboxId(), opID: msg.GetExecId(), kinds: attachKinds}
	return r.serverStream(ctx, req.Header(), info, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_ExecAttachRequest{ExecAttachRequest: msg}
	}, execEmitter(stream))
}

func (r RelayService) ReadFile(ctx context.Context, req *connect.Request[ownsandv1.ReadFileRequest], stream *connect.ServerStream[ownsandv1.ReadFileResponse]) error {
	msg := req.Msg
	info := rpcInfo{procedure: ownsandv1connect.RelayServiceReadFileProcedure, org: msg.GetOrganizationId(), sandbox: msg.GetSandboxId(), opID: msg.GetReadId(),
		kinds: []ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE}}
	return r.serverStream(ctx, req.Header(), info, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileReadRequest{FileReadRequest: msg}
	}, emitter{
		accepted: func() error {
			return stream.Send(&ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Accepted_{Accepted: &ownsandv1.ReadFileResponse_Accepted{}}})
		},
		keepalive: func() error {
			return stream.Send(&ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Keepalive_{Keepalive: &ownsandv1.ReadFileResponse_Keepalive{}}})
		},
		payload: func(f *ownsandv1.DataFrame) (bool, error) {
			switch p := f.GetPayload().(type) {
			case *ownsandv1.DataFrame_FileChunk:
				return false, stream.Send(&ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Chunk{Chunk: p.FileChunk}})
			case *ownsandv1.DataFrame_FileReadFinished:
				return true, stream.Send(&ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Finished_{Finished: p.FileReadFinished}})
			}
			return false, nil
		},
	})
}

// WriteFile forwards one chunk and returns the agent's result. The whole
// call, including the request upload and the agent's result, must finish
// within the WriteFile deadline.
func (r RelayService) WriteFile(ctx context.Context, req *connect.Request[ownsandv1.WriteFileRequest]) (*connect.Response[ownsandv1.WriteFileResponse], error) {
	m := r.M
	start := time.Now()
	msg := req.Msg
	info := rpcInfo{procedure: ownsandv1connect.RelayServiceWriteFileProcedure, org: msg.GetOrganizationId(), sandbox: msg.GetSandboxId(), opID: msg.GetWriteId(),
		kinds: []ownsandv1.RelayOperationKind{ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE}}
	ctx, cancel := context.WithTimeout(ctx, m.Limits.WriteFileDeadline)
	defer cancel()
	var result *ownsandv1.WriteFileResponse
	st, _, err := m.begin(ctx, req.Header(), info, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileWriteChunk{FileWriteChunk: msg}
	})
	if err == nil {
		err = func() error {
			defer m.endRPC(st)
			for {
				for f := st.pop(); f != nil; f = st.pop() {
					if e := agentFrameError(f); e != nil {
						st.forget()
						return e
					}
					if res := f.GetFileWriteResult(); res != nil {
						st.framesIn++
						st.bytesIn += datapath.FrameSize(f)
						result = res
						st.closeStream()
						return nil
					}
				}
				select {
				case <-st.sig:
				case <-st.failCh:
					return st.failErr
				case <-ctx.Done():
					st.closeStream()
					if errors.Is(ctx.Err(), context.DeadlineExceeded) {
						return connect.NewError(connect.CodeDeadlineExceeded, errors.New("WriteFile did not finish within its deadline"))
					}
					return ctx.Err()
				}
			}
		}()
	}
	m.logRPC(ctx, info.procedure, st, info, start, err)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(result), nil
}
