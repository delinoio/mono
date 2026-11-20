package ownsandtest

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Relay timing of the control plane contract.
const (
	grantLifetime     = 15 * time.Minute
	admissionTolerant = 30 * time.Second
	grantIdleRelease  = 60 * time.Second
	streamRotation    = 14 * time.Minute
	keepaliveInterval = 60 * time.Second
	authorizePerSec   = 10
)

var (
	errRotate      = errors.New("stream rotation")
	errReplaced    = errors.New("grant replaced")
	errChannelLost = errors.New("agent channel lost")
	errAbortStream = errors.New("abort stream")
	errFaultAction = errors.New("fault action")
)

// AuthorizeCall records one AuthorizeRelay call.
type AuthorizeCall struct {
	Kind            ownsandv1.RelayOperationKind
	OperationID     string
	AuthorizationID string
	// ReplacesGrantID is empty when the request named none.
	ReplacesGrantID string
	Digest          []byte
	At              time.Time
	// N is the 1-based number of AuthorizeRelay calls for this operation.
	N int
}

// RelayRPC names a RelayService RPC.
type RelayRPC int

const (
	RelayExec RelayRPC = iota + 1
	RelayAttachExec
	RelayReadFile
	RelayWriteFile
)

// RelayCall records one RelayService call.
type RelayCall struct {
	RPC         RelayRPC
	GrantID     string
	OperationID string
	// Offset is the read offset, the write chunk offset or the stdout offset.
	Offset       int64
	StderrOffset int64
	Finish       bool
	At           time.Time
	// N is the 1-based number of relay calls for this operation.
	N int
}

type fakeGrant struct {
	id         string
	org        string
	sandbox    string
	principal  Principal
	kind       ownsandv1.RelayOperationKind
	opID       string
	digest     []byte
	expiresAt  time.Time
	lastActive time.Time
	streams    map[int]context.CancelCauseFunc
	invalid    bool
	replaced   bool
}

func (g *fakeGrant) admittable(now time.Time) bool {
	if g.invalid || g.replaced || now.After(g.expiresAt.Add(admissionTolerant)) {
		return false
	}
	if len(g.streams) == 0 && now.Sub(g.lastActive) > grantIdleRelease {
		// The agent released the idle grant.
		g.invalid = true
		return false
	}
	return true
}

type dataPathSvc struct{ s *Server }

func (h *dataPathSvc) GetIceServers(ctx context.Context, req *connect.Request[ownsandv1.GetIceServersRequest]) (*connect.Response[ownsandv1.GetIceServersResponse], error) {
	if _, err := h.s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.GetIceServersResponse{}), nil
}

func (h *dataPathSvc) OpenSession(ctx context.Context, req *connect.Request[ownsandv1.OpenSessionRequest]) (*connect.Response[ownsandv1.OpenSessionResponse], error) {
	s := h.s
	s.mu.Lock()
	s.openSession++
	n := s.openSession
	s.mu.Unlock()
	if s.OnOpenSession != nil {
		s.OnOpenSession(n)
	}
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	switch s.state(req.Msg.GetSandboxId()) {
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
	case ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("sandbox unreachable"))
	case ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED:
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox not running"))
	}
	if s.p2p == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("peer-to-peer is not enabled on the fake agent"))
	}
	answer, err := s.p2p.answer(ctx, req.Msg.GetSdpOffer(), req.Msg.GetSandboxId())
	if err != nil {
		return nil, connect.NewError(connect.CodeUnavailable, err)
	}
	return connect.NewResponse(&ownsandv1.OpenSessionResponse{SessionId: datapath.NewID(), SdpAnswer: answer,
		ExpireTime: timestamppb.New(s.Clock.Now().Add(time.Hour))}), nil
}

func (h *dataPathSvc) AuthorizeRelay(ctx context.Context, req *connect.Request[ownsandv1.AuthorizeRelayRequest]) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	s := h.s
	m := req.Msg
	now := s.Clock.Now()
	call := AuthorizeCall{Kind: m.GetOperationKind(), OperationID: m.GetOperationId(), AuthorizationID: m.GetAuthorizationId(),
		ReplacesGrantID: m.GetReplacesGrantId(), Digest: m.GetRequestDigest(), At: now}
	s.mu.Lock()
	for _, c := range s.authorizes {
		if c.OperationID == call.OperationID {
			call.N++
		}
	}
	call.N++
	s.authorizes = append(s.authorizes, call)
	s.mu.Unlock()
	var f *Fault
	if s.OnAuthorize != nil {
		f = s.OnAuthorize(&call)
	}
	if f != nil && f.Err != nil {
		return nil, f.Err
	}
	if f != nil && f.Abort {
		panic(http.ErrAbortHandler)
	}
	if f != nil && f.Delay > 0 {
		if err := clock.Sleep(ctx, s.Clock, f.Delay); err != nil {
			return nil, err
		}
		now = s.Clock.Now()
	}
	p, err := s.checkOrg(req.Header(), m.GetOrganizationId())
	if err != nil {
		return nil, err
	}
	if !datapath.IsUUIDv7(m.GetAuthorizationId()) || !datapath.IsUUIDv7(m.GetOperationId()) {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("authorization and operation IDs must be UUID v7"))
	}
	hasDigest := len(m.GetRequestDigest()) > 0
	if (m.GetOperationKind() == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC) == hasDigest {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("request digest required except for ATTACH_EXEC"))
	}
	s.mu.Lock()
	if gid, ok := s.authzByID[m.GetAuthorizationId()]; ok {
		if g := s.grants[gid]; g != nil && g.admittable(now) && g.principal == p {
			s.mu.Unlock()
			return s.grantResponse(g, now, f)
		}
	}
	var recent []time.Time
	for _, t := range s.authRate[p.ID] {
		if now.Sub(t) < time.Second {
			recent = append(recent, t)
		}
	}
	if len(recent) >= authorizePerSec {
		s.authRate[p.ID] = recent
		s.mu.Unlock()
		return nil, RetryError(time.Second)
	}
	s.authRate[p.ID] = append(recent, now)
	if !s.agentOnline {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("no agent channel for the device"))
	}
	sb := s.sandboxes[m.GetSandboxId()]
	if sb == nil {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	st := sb.State
	okState := st == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
	if k := m.GetOperationKind(); k == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || k == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC {
		okState = okState || st == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED
	}
	if st == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("sandbox unreachable"))
	}
	if !okState {
		s.mu.Unlock()
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox state does not allow the operation"))
	}
	var cancels []context.CancelCauseFunc
	if r := m.GetReplacesGrantId(); r != "" {
		if old := s.grants[r]; old != nil && old.principal == p && old.opID == m.GetOperationId() && old.kind == m.GetOperationKind() && bytes.Equal(old.digest, m.GetRequestDigest()) && !old.invalid {
			old.replaced = true
			for _, c := range old.streams {
				cancels = append(cancels, c)
			}
		}
	}
	life := s.GrantLifetime
	if life == 0 {
		life = grantLifetime
	}
	g := &fakeGrant{id: datapath.NewUUIDv7At(now).String(), org: m.GetOrganizationId(), sandbox: m.GetSandboxId(), principal: p,
		kind: m.GetOperationKind(), opID: m.GetOperationId(), digest: m.GetRequestDigest(), expiresAt: now.Add(life),
		lastActive: now, streams: map[int]context.CancelCauseFunc{}}
	s.grants[g.id] = g
	s.authzByID[m.GetAuthorizationId()] = g.id
	s.mu.Unlock()
	for _, c := range cancels {
		c(errReplaced)
	}
	if f != nil && f.RevokeAfter {
		s.RevokeCredential(strings.TrimPrefix(req.Header().Get("Authorization"), "Bearer "))
	}
	return s.grantResponse(g, now, f)
}

func (s *Server) grantResponse(g *fakeGrant, now time.Time, f *Fault) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	if f != nil && f.LoseResponse {
		panic(http.ErrAbortHandler)
	}
	return connect.NewResponse(&ownsandv1.AuthorizeRelayResponse{
		GrantId:   g.id,
		ExpiresAt: timestamppb.New(g.expiresAt.Add(s.ExpiresAtOffset)),
		ExpiresIn: durationpb.New(g.expiresAt.Sub(now)),
	}), nil
}

// admit checks the caller and the grant of a relay RPC, records the call
// and registers the stream on the grant.
func (s *Server) admit(ctx context.Context, hdr http.Header, rc *RelayCall, org, sandbox string, kinds ...ownsandv1.RelayOperationKind) (*fakeGrant, context.Context, func(), error) {
	now := s.Clock.Now()
	rc.At = now
	rc.GrantID = hdr.Get(datapath.RelayGrantHeader)
	s.mu.Lock()
	for _, c := range s.relayCalls {
		if c.OperationID == rc.OperationID {
			rc.N++
		}
	}
	rc.N++
	s.relayCalls = append(s.relayCalls, *rc)
	s.mu.Unlock()
	p, err := s.principal(hdr)
	if err != nil {
		return nil, nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.agentOnline {
		return nil, nil, nil, connect.NewError(connect.CodeUnavailable, errors.New("no agent relay connection"))
	}
	g := s.grants[rc.GrantID]
	if g == nil || !g.admittable(now) {
		return nil, nil, nil, GrantInvalidError()
	}
	kindOK := false
	for _, k := range kinds {
		kindOK = kindOK || g.kind == k
	}
	if g.principal != p || g.org != org || g.sandbox != sandbox || g.opID != rc.OperationID || !kindOK {
		return nil, nil, nil, connect.NewError(connect.CodePermissionDenied, errors.New("request does not match the relay grant"))
	}
	sctx, cancel := context.WithCancelCause(ctx)
	s.nextStream++
	id := s.nextStream
	g.streams[id] = cancel
	release := func() {
		s.mu.Lock()
		delete(g.streams, id)
		g.lastActive = s.Clock.Now()
		s.mu.Unlock()
		cancel(context.Canceled)
	}
	return g, sctx, release, nil
}

func (s *Server) fault(rc *RelayCall) *Fault {
	if s.OnRelay == nil {
		return nil
	}
	f := s.OnRelay(rc)
	return f
}

// causeError maps the cause of an ended relay stream to its result.
func causeError(ctx context.Context, err error) error {
	switch cause := context.Cause(ctx); {
	case errors.Is(cause, errRotate):
		return nil
	case errors.Is(cause, errReplaced):
		return sessionEndError(ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_REPLACED)
	case errors.Is(cause, errChannelLost):
		return ChannelLostError()
	case errors.Is(cause, errAbortStream):
		panic(http.ErrAbortHandler)
	}
	return err
}

// serveStream runs one relay server stream: keepalives, the rotation at
// 14 minutes, the accepted message and the injected fault.
func serveStream[T any](s *Server, sctx context.Context, f *Fault, stream *connect.ServerStream[T], accepted, keepalive func() *T,
	run func(ctx context.Context, accept func() error, send func(*T) error) error) error {
	if f == nil {
		f = &Fault{}
	}
	if f.SlowWrite > 0 {
		setSlow(sctx, f.SlowWrite)
	}
	var mu sync.Mutex
	lastSend := s.Clock.Now()
	stalled := false
	send := func(m *T) error {
		mu.Lock()
		defer mu.Unlock()
		lastSend = s.Clock.Now()
		return stream.Send(m)
	}
	if f.KeepaliveFirst {
		if err := send(keepalive()); err != nil {
			return err
		}
	}
	if f.Delay > 0 {
		if err := clock.Sleep(sctx, s.Clock, f.Delay); err != nil {
			return causeError(sctx, err)
		}
	}
	if f.Stall {
		<-sctx.Done()
		return causeError(sctx, sctx.Err())
	}
	if f.Err != nil {
		return f.Err
	}
	if f.Abort {
		panic(http.ErrAbortHandler)
	}
	if f.EndBeforeAccepted {
		return nil
	}
	rctx, cancelRot := clock.WithTimeout(sctx, s.Clock, streamRotation, errRotate)
	defer cancelRot()
	kaDone := make(chan struct{})
	var kaWG sync.WaitGroup
	kaWG.Add(1)
	go func() {
		defer kaWG.Done()
		for {
			t := s.Clock.NewTimer(keepaliveInterval / 4)
			select {
			case <-kaDone:
				t.Stop()
				return
			case <-rctx.Done():
				t.Stop()
				return
			case <-t.C():
			}
			mu.Lock()
			if !stalled && s.Clock.Now().Sub(lastSend) >= keepaliveInterval {
				lastSend = s.Clock.Now()
				_ = stream.Send(keepalive())
			}
			mu.Unlock()
		}
	}()
	defer func() {
		close(kaDone)
		kaWG.Wait()
	}()
	count := 0
	trigger := func() error {
		if f.After == ActionNone {
			return nil
		}
		if f.Hold > 0 {
			if err := clock.Sleep(rctx, s.Clock, f.Hold); err != nil {
				return err
			}
		}
		return errFaultAction
	}
	accept := func() error {
		if err := send(accepted()); err != nil {
			return err
		}
		if f.AfterMessages == 0 {
			return trigger()
		}
		return nil
	}
	agentSend := func(m *T) error {
		if err := send(m); err != nil {
			return err
		}
		count++
		if f.AfterMessages > 0 && count == f.AfterMessages {
			return trigger()
		}
		return nil
	}
	err := run(rctx, accept, agentSend)
	if errors.Is(err, errFaultAction) {
		switch f.After {
		case ActionAbort:
			panic(http.ErrAbortHandler)
		case ActionRotate:
			return nil
		case ActionStall:
			mu.Lock()
			stalled = true
			mu.Unlock()
			<-sctx.Done()
			return causeError(sctx, sctx.Err())
		case ActionChannelLost:
			return ChannelLostError()
		}
	}
	if rctx.Err() != nil {
		return causeError(rctx, err)
	}
	return err
}

type relaySvc struct{ s *Server }

func execAccepted() *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Accepted_{Accepted: &ownsandv1.ExecEvent_Accepted{}}}
}

func execKeepalive() *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive_{Keepalive: &ownsandv1.ExecEvent_Keepalive{}}}
}

func (h *relaySvc) Exec(ctx context.Context, req *connect.Request[ownsandv1.ExecRequest], stream *connect.ServerStream[ownsandv1.ExecEvent]) error {
	s := h.s
	m := req.Msg
	rc := &RelayCall{RPC: RelayExec, OperationID: m.GetExecId()}
	g, sctx, release, err := s.admit(ctx, req.Header(), rc, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC)
	if err != nil {
		return err
	}
	defer release()
	if !bytes.Equal(datapath.ExecDigest(m), g.digest) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("request digest does not match the grant"))
	}
	return serveStream(s, sctx, s.fault(rc), stream, execAccepted, execKeepalive, func(ctx context.Context, accept func() error, send func(*ownsandv1.ExecEvent) error) error {
		return s.Agent.Exec(ctx, m, accept, send)
	})
}

func (h *relaySvc) AttachExec(ctx context.Context, req *connect.Request[ownsandv1.AttachExecRequest], stream *connect.ServerStream[ownsandv1.ExecEvent]) error {
	s := h.s
	m := req.Msg
	rc := &RelayCall{RPC: RelayAttachExec, OperationID: m.GetExecId(), Offset: m.GetStdoutOffset(), StderrOffset: m.GetStderrOffset()}
	g, sctx, release, err := s.admit(ctx, req.Header(), rc, m.GetOrganizationId(), m.GetSandboxId(),
		ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC)
	if err != nil {
		return err
	}
	defer release()
	return serveStream(s, sctx, s.fault(rc), stream, execAccepted, execKeepalive, func(ctx context.Context, accept func() error, send func(*ownsandv1.ExecEvent) error) error {
		return s.Agent.Attach(ctx, m, g.kind, g.digest, accept, send)
	})
}

func (h *relaySvc) ReadFile(ctx context.Context, req *connect.Request[ownsandv1.ReadFileRequest], stream *connect.ServerStream[ownsandv1.ReadFileResponse]) error {
	s := h.s
	m := req.Msg
	rc := &RelayCall{RPC: RelayReadFile, OperationID: m.GetReadId(), Offset: m.GetOffset()}
	g, sctx, release, err := s.admit(ctx, req.Header(), rc, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE)
	if err != nil {
		return err
	}
	defer release()
	if !bytes.Equal(datapath.ReadDigest(m), g.digest) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("request digest does not match the grant"))
	}
	accepted := func() *ownsandv1.ReadFileResponse {
		return &ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Accepted_{Accepted: &ownsandv1.ReadFileResponse_Accepted{}}}
	}
	keepalive := func() *ownsandv1.ReadFileResponse {
		return &ownsandv1.ReadFileResponse{Message: &ownsandv1.ReadFileResponse_Keepalive_{Keepalive: &ownsandv1.ReadFileResponse_Keepalive{}}}
	}
	return serveStream(s, sctx, s.fault(rc), stream, accepted, keepalive, func(ctx context.Context, accept func() error, send func(*ownsandv1.ReadFileResponse) error) error {
		return s.Agent.Read(ctx, m, accept, send)
	})
}

func (h *relaySvc) WriteFile(ctx context.Context, req *connect.Request[ownsandv1.WriteFileRequest]) (*connect.Response[ownsandv1.WriteFileResponse], error) {
	s := h.s
	m := req.Msg
	rc := &RelayCall{RPC: RelayWriteFile, OperationID: m.GetWriteId(), Offset: m.GetChunk().GetOffset(), Finish: m.GetFinish() != nil}
	g, sctx, release, err := s.admit(ctx, req.Header(), rc, m.GetOrganizationId(), m.GetSandboxId(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE)
	if err != nil {
		return nil, err
	}
	defer release()
	f := s.fault(rc)
	if f != nil {
		if f.Delay > 0 {
			if err := clock.Sleep(sctx, s.Clock, f.Delay); err != nil {
				return nil, causeError(sctx, err)
			}
		}
		if f.Err != nil {
			return nil, f.Err
		}
		if f.Abort {
			panic(http.ErrAbortHandler)
		}
		if f.Stall {
			<-sctx.Done()
			return nil, causeError(sctx, sctx.Err())
		}
	}
	resp, err := s.Agent.Write(m, g.digest, true)
	if err != nil {
		return nil, err
	}
	if f != nil && f.LoseResponse {
		panic(http.ErrAbortHandler)
	}
	return connect.NewResponse(resp), nil
}
