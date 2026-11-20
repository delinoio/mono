// Package ownsandtest is an in-process fake of the OwnSand control plane,
// its relay and a device agent, for tests of the Go SDK and the CLI.
//
// The fake serves the Connect handlers of the public services and of
// RelayService on an httptest server that speaks HTTP/1.1 and h2c. The relay
// handlers call a fake agent in memory instead of a RelayAgentService
// connection, and the fake agent can also answer a peer-to-peer data channel
// with pion. Tests inject faults (lost responses, transport interruptions,
// rotations, stalls, capacity rejections) with the On* hooks and move the
// shared scaled clock with Clock.Advance.
//
// The fake is not the control plane: it implements only the behavior that
// the SDK and CLI rules depend on.
package ownsandtest

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/clock"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// DefaultAPIKey is the API key that New accepts.
const DefaultAPIKey = "osk_fake-test-key-0123456789"

// Principal is the identity of a credential.
type Principal struct {
	Kind ownsandv1.PrincipalKind
	ID   string
}

// Options configure New.
type Options struct {
	// Scale is the clock scale; zero means 50 (one virtual minute in 1.2 s).
	Scale float64
}

// Server is the fake control plane.
type Server struct {
	t     testing.TB
	URL   string
	Clock *clock.Scaled
	// Org is the organization of the default API key.
	Org string
	// Agent is the fake device agent behind the relay.
	Agent *Agent

	hs *httptest.Server

	mu          sync.Mutex
	creds       map[string]Principal
	sandboxes   map[string]*ownsandv1.Sandbox
	creates     map[string]createRecord
	grants      map[string]*fakeGrant
	authzByID   map[string]string
	authorizes  []AuthorizeCall
	relayCalls  []RelayCall
	authRate    map[string][]time.Time
	openSession int
	agentOnline bool
	nextStream  int
	// GrantLifetime is the lifetime of new relay grants (default 15 minutes).
	GrantLifetime time.Duration
	// ExpiresAtOffset shifts the expires_at that AuthorizeRelay returns, to
	// simulate a caller whose wall clock differs from the server's.
	ExpiresAtOffset time.Duration
	// SeenCredentials records the Authorization header values that reached
	// the server.
	SeenCredentials []string

	// OnAuthorize may return a fault for an AuthorizeRelay call.
	OnAuthorize func(*AuthorizeCall) *Fault
	// OnRelay may return a fault for a RelayService call.
	OnRelay func(*RelayCall) *Fault
	// OnOpenSession is called at the start of the n-th OpenSession call; it
	// may block to order a test.
	OnOpenSession func(n int)
	// OnCreateSandbox may return a fault for a CreateSandbox call.
	OnCreateSandbox func(*ownsandv1.CreateSandboxRequest) *Fault

	p2p *peerAgent
	mgmt
}

type createRecord struct {
	req *ownsandv1.CreateSandboxRequest
	sb  *ownsandv1.Sandbox
}

// New starts a fake control plane with one organization, one API key and
// no sandboxes. The server stops at the end of the test.
func New(t testing.TB, opts Options) *Server {
	t.Helper()
	scale := opts.Scale
	if scale == 0 {
		scale = 50
	}
	s := &Server{
		t:           t,
		Clock:       clock.NewScaled(scale),
		Org:         datapath.NewID(),
		creds:       map[string]Principal{},
		sandboxes:   map[string]*ownsandv1.Sandbox{},
		creates:     map[string]createRecord{},
		grants:      map[string]*fakeGrant{},
		authzByID:   map[string]string{},
		authRate:    map[string][]time.Time{},
		agentOnline: true,
	}
	s.creds[DefaultAPIKey] = Principal{Kind: ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY, ID: "api-key-1"}
	s.Agent = newAgent(s)
	s.initMgmt()
	mux := http.NewServeMux()
	mux.Handle(ownsandv1connect.NewOrganizationServiceHandler(&orgSvc{s}))
	mux.Handle(ownsandv1connect.NewMemberServiceHandler(&memberSvc{s}))
	mux.Handle(ownsandv1connect.NewApiKeyServiceHandler(&apiKeySvc{s}))
	mux.Handle(ownsandv1connect.NewDeviceServiceHandler(&deviceSvc{s}))
	mux.Handle(ownsandv1connect.NewSandboxServiceHandler(&sandboxSvc{s}))
	mux.Handle(ownsandv1connect.NewSnapshotServiceHandler(&snapshotSvc{s}))
	mux.Handle(ownsandv1connect.NewDataPathServiceHandler(&dataPathSvc{s}))
	mux.Handle(ownsandv1connect.NewRelayServiceHandler(&relaySvc{s}))
	hs := httptest.NewUnstartedServer(slowMiddleware(s, mux))
	hs.Config.Protocols = new(http.Protocols)
	hs.Config.Protocols.SetHTTP1(true)
	hs.Config.Protocols.SetUnencryptedHTTP2(true)
	// Handler panics with http.ErrAbortHandler simulate transport
	// interruptions; keep server noise out of the test log.
	hs.Config.ErrorLog = log.New(io.Discard, "", 0)
	hs.Start()
	s.hs = hs
	s.URL = hs.URL
	t.Cleanup(func() {
		if s.p2p != nil {
			s.p2p.closeAll()
		}
		hs.CloseClientConnections()
		hs.Close()
	})
	return s
}

// AddCredential accepts cred as a credential of p.
func (s *Server) AddCredential(cred string, p Principal) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creds[cred] = p
}

// RevokeCredential stops accepting cred.
func (s *Server) RevokeCredential(cred string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.creds, cred)
}

func (s *Server) principal(h http.Header) (Principal, error) {
	v := h.Get("Authorization")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.SeenCredentials = append(s.SeenCredentials, v)
	cred, ok := strings.CutPrefix(v, "Bearer ")
	if !ok {
		return Principal{}, connect.NewError(connect.CodeUnauthenticated, errors.New("missing credential"))
	}
	p, ok := s.creds[cred]
	if !ok {
		return Principal{}, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credential"))
	}
	return p, nil
}

// checkOrg authorizes p for org.
func (s *Server) checkOrg(h http.Header, org string) (Principal, error) {
	p, err := s.principal(h)
	if err != nil {
		return p, err
	}
	if org != s.Org {
		return p, connect.NewError(connect.CodeNotFound, errors.New("organization not found"))
	}
	return p, nil
}

// AddSandbox adds a sandbox in state and returns it.
func (s *Server) AddSandbox(state ownsandv1.SandboxState) *ownsandv1.Sandbox {
	sb := &ownsandv1.Sandbox{OrganizationId: s.Org, SandboxId: datapath.NewID(), DeviceId: "device-1", State: state,
		Resources: &ownsandv1.Resources{VcpuCount: 1, MemoryMib: 512, DiskGib: 4}}
	s.mu.Lock()
	s.sandboxes[sb.SandboxId] = sb
	s.mu.Unlock()
	return proto.Clone(sb).(*ownsandv1.Sandbox)
}

// SetState changes the state of a sandbox and wakes the fake agent.
func (s *Server) SetState(id string, state ownsandv1.SandboxState, reason ownsandv1.SandboxEndReason) {
	s.mu.Lock()
	if sb := s.sandboxes[id]; sb != nil {
		sb.State = state
		sb.EndReason = reason
	}
	s.mu.Unlock()
	s.Agent.wakeAll()
}

func (s *Server) state(id string) ownsandv1.SandboxState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sb := s.sandboxes[id]; sb != nil {
		return sb.State
	}
	return ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
}

// Fault describes an injected failure. Fields apply where the RPC supports them.
type Fault struct {
	// Err ends the call with this error (a valid terminal status).
	Err error
	// Abort resets the HTTP/2 stream, so the client sees a transport
	// interruption without a terminal status.
	Abort bool
	// LoseResponse applies the call (installs the grant, writes the chunk)
	// and then resets the stream, so the response is lost.
	LoseResponse bool
	// KeepaliveFirst sends a relay keepalive before the fault or the agent.
	KeepaliveFirst bool
	// EndBeforeAccepted ends the stream with OK before the agent accepts it.
	EndBeforeAccepted bool
	// AfterMessages forwards this many agent messages after accepted before
	// the After action applies. Zero applies it right after accepted.
	AfterMessages int
	// After is the action after accepted and AfterMessages messages.
	After Action
	// Hold keeps the stream open this long (virtual, with keepalives)
	// before the After action.
	Hold time.Duration
	// Stall stops every byte, also keepalives, before the agent accepts the
	// stream (or before the response of a WriteFile), until the client
	// gives up.
	Stall bool
	// SlowWrite trickles the response body in 64 KiB pieces with this
	// virtual pause between them.
	SlowWrite time.Duration
	// Delay waits this long (virtual) before the call is handled; on a relay
	// stream it waits after KeepaliveFirst and before the agent accepts.
	Delay time.Duration
	// RevokeAfter revokes the caller's credential after AuthorizeRelay
	// answered.
	RevokeAfter bool
}

// Action is a fault action after accepted.
type Action int

const (
	// ActionNone forwards the agent normally.
	ActionNone Action = iota
	// ActionAbort resets the stream (transport interruption).
	ActionAbort
	// ActionRotate ends the stream with OK and no final event.
	ActionRotate
	// ActionStall sends nothing more, also no keepalive.
	ActionStall
	// ActionChannelLost ends the stream with unavailable and AGENT_CHANNEL_LOST.
	ActionChannelLost
)

func sessionEndError(reason ownsandv1.SessionEndReason) error {
	e := connect.NewError(connect.CodeUnavailable, errors.New("relay stream ended: "+reason.String()))
	if d, err := connect.NewErrorDetail(&ownsandv1.SessionEndDetail{Reason: reason}); err == nil {
		e.AddDetail(d)
	}
	return e
}

// RetryError returns a resource_exhausted error with a retry delay detail.
func RetryError(delay time.Duration) error {
	e := connect.NewError(connect.CodeResourceExhausted, errors.New("capacity reached"))
	if d, err := connect.NewErrorDetail(&ownsandv1.RetryDetail{RetryDelay: durationpb.New(delay)}); err == nil {
		e.AddDetail(d)
	}
	return e
}

// GrantInvalidError returns the unavailable error with GRANT_INVALID.
func GrantInvalidError() error {
	return sessionEndError(ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID)
}

// ChannelLostError returns the unavailable error with AGENT_CHANNEL_LOST.
func ChannelLostError() error {
	return sessionEndError(ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST)
}

// SetAgentOnline simulates the agent channel. Offline ends every relay
// stream with AGENT_CHANNEL_LOST, ends every grant, makes AuthorizeRelay
// fail with unavailable and marks running sandboxes UNREACHABLE.
func (s *Server) SetAgentOnline(online bool) {
	s.mu.Lock()
	s.agentOnline = online
	var cancels []context.CancelCauseFunc
	if !online {
		for _, g := range s.grants {
			g.invalid = true
			for _, c := range g.streams {
				cancels = append(cancels, c)
			}
		}
		for _, sb := range s.sandboxes {
			if sb.State == ownsandv1.SandboxState_SANDBOX_STATE_RUNNING || sb.State == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
				sb.LastKnownState = sb.State
				sb.State = ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE
			}
		}
	} else {
		for _, sb := range s.sandboxes {
			if sb.State == ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE {
				sb.State = sb.LastKnownState
				sb.LastKnownState = ownsandv1.SandboxState_SANDBOX_STATE_UNSPECIFIED
			}
		}
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c(errChannelLost)
	}
	s.Agent.wakeAll()
}

// Restart simulates a control plane restart: every open relay RPC ends
// with a transport interruption and every grant ends.
func (s *Server) Restart() {
	s.mu.Lock()
	var cancels []context.CancelCauseFunc
	for _, g := range s.grants {
		g.invalid = true
		for _, c := range g.streams {
			cancels = append(cancels, c)
		}
	}
	s.mu.Unlock()
	for _, c := range cancels {
		c(errAbortStream)
	}
}

// AuthorizeCalls returns the AuthorizeRelay calls so far.
func (s *Server) AuthorizeCalls() []AuthorizeCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]AuthorizeCall(nil), s.authorizes...)
}

// RelayCalls returns the RelayService calls so far.
func (s *Server) RelayCalls() []RelayCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]RelayCall(nil), s.relayCalls...)
}

// OpenSessionCalls returns the number of OpenSession calls.
func (s *Server) OpenSessionCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.openSession
}

// sandboxSvc implements SandboxService.
type sandboxSvc struct{ s *Server }

func (h *sandboxSvc) CreateSandbox(ctx context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	m := req.Msg
	if m.GetIdleTtl() == nil || m.GetMaxTtl() == nil || m.GetResources().GetVcpuCount() <= 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("missing TTL or resources"))
	}
	var f *Fault
	if s.OnCreateSandbox != nil {
		f = s.OnCreateSandbox(m)
	}
	if f != nil && f.Err != nil {
		return nil, f.Err
	}
	if f != nil && f.Abort {
		panic(http.ErrAbortHandler)
	}
	s.mu.Lock()
	rec, ok := s.creates[m.GetIdempotencyKey()]
	if ok {
		s.mu.Unlock()
		if !proto.Equal(rec.req, m) {
			return nil, connect.NewError(connect.CodeAlreadyExists, errors.New("idempotency key reused with other parameters"))
		}
		return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: proto.Clone(rec.sb).(*ownsandv1.Sandbox)}), nil
	}
	sb := &ownsandv1.Sandbox{OrganizationId: s.Org, SandboxId: datapath.NewID(), DeviceId: "device-1",
		State: ownsandv1.SandboxState_SANDBOX_STATE_CREATING, Source: m.GetSource(), Resources: m.GetResources(),
		IdleTtl: m.GetIdleTtl(), MaxTtl: m.GetMaxTtl(), Labels: m.GetLabels(), Network: m.GetNetwork(),
		CreateTime: timestamppb.New(s.Clock.Now())}
	s.sandboxes[sb.SandboxId] = sb
	s.creates[m.GetIdempotencyKey()] = createRecord{req: proto.Clone(m).(*ownsandv1.CreateSandboxRequest), sb: proto.Clone(sb).(*ownsandv1.Sandbox)}
	s.mu.Unlock()
	if f != nil && f.LoseResponse {
		panic(http.ErrAbortHandler)
	}
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: proto.Clone(sb).(*ownsandv1.Sandbox)}), nil
}

// CreateCount returns the number of distinct sandboxes that CreateSandbox made.
func (s *Server) CreateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.creates)
}

func (h *sandboxSvc) get(hdr http.Header, org, id string) (*ownsandv1.Sandbox, error) {
	s := h.s
	if _, err := s.checkOrg(hdr, org); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	sb := s.sandboxes[id]
	if sb == nil {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	return sb, nil
}

func (h *sandboxSvc) GetSandbox(ctx context.Context, req *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	sb, err := h.get(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSandboxId())
	if err != nil {
		return nil, err
	}
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: proto.Clone(sb).(*ownsandv1.Sandbox)}), nil
}

func (h *sandboxSvc) ListSandboxes(ctx context.Context, req *connect.Request[ownsandv1.ListSandboxesRequest]) (*connect.Response[ownsandv1.ListSandboxesResponse], error) {
	s := h.s
	if _, err := s.checkOrg(req.Header(), req.Msg.GetOrganizationId()); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resp := &ownsandv1.ListSandboxesResponse{}
	for _, sb := range s.sandboxes {
		resp.Sandboxes = append(resp.Sandboxes, proto.Clone(sb).(*ownsandv1.Sandbox))
	}
	return connect.NewResponse(resp), nil
}

func (h *sandboxSvc) setState(hdr http.Header, org, id string, to ownsandv1.SandboxState) (*ownsandv1.Sandbox, error) {
	sb, err := h.get(hdr, org, id)
	if err != nil {
		return nil, err
	}
	reason := ownsandv1.SandboxEndReason_SANDBOX_END_REASON_UNSPECIFIED
	if to == ownsandv1.SandboxState_SANDBOX_STATE_DELETED {
		reason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
	}
	h.s.SetState(id, to, reason)
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	return proto.Clone(sb).(*ownsandv1.Sandbox), nil
}

func (h *sandboxSvc) DeleteSandbox(ctx context.Context, req *connect.Request[ownsandv1.DeleteSandboxRequest]) (*connect.Response[ownsandv1.DeleteSandboxResponse], error) {
	sb, err := h.setState(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.DeleteSandboxResponse{Sandbox: sb}), nil
}

func (h *sandboxSvc) PauseSandbox(ctx context.Context, req *connect.Request[ownsandv1.PauseSandboxRequest]) (*connect.Response[ownsandv1.PauseSandboxResponse], error) {
	sb, err := h.setState(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.PauseSandboxResponse{Sandbox: sb}), nil
}

func (h *sandboxSvc) ResumeSandbox(ctx context.Context, req *connect.Request[ownsandv1.ResumeSandboxRequest]) (*connect.Response[ownsandv1.ResumeSandboxResponse], error) {
	sb, err := h.setState(req.Header(), req.Msg.GetOrganizationId(), req.Msg.GetSandboxId(), ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&ownsandv1.ResumeSandboxResponse{Sandbox: sb}), nil
}

// slowCtl lets a relay handler trickle its response body.
type slowCtl struct {
	mu    sync.Mutex
	delay time.Duration
}

type slowKey struct{}

func slowMiddleware(s *Server, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctl := &slowCtl{}
		r = r.WithContext(context.WithValue(r.Context(), slowKey{}, ctl))
		next.ServeHTTP(&slowWriter{ResponseWriter: w, ctl: ctl, s: s, ctx: r.Context()}, r)
	})
}

func setSlow(ctx context.Context, d time.Duration) {
	if ctl, ok := ctx.Value(slowKey{}).(*slowCtl); ok {
		ctl.mu.Lock()
		ctl.delay = d
		ctl.mu.Unlock()
	}
}

type slowWriter struct {
	http.ResponseWriter
	ctl *slowCtl
	s   *Server
	ctx context.Context
}

func (w *slowWriter) Write(p []byte) (int, error) {
	w.ctl.mu.Lock()
	d := w.ctl.delay
	w.ctl.mu.Unlock()
	if d <= 0 {
		return w.ResponseWriter.Write(p)
	}
	const piece = 64 << 10
	n := 0
	for len(p) > 0 {
		k := min(len(p), piece)
		m, err := w.ResponseWriter.Write(p[:k])
		n += m
		if err != nil {
			return n, err
		}
		if f, ok := w.ResponseWriter.(http.Flusher); ok {
			f.Flush()
		}
		p = p[k:]
		if len(p) > 0 {
			if err := clock.Sleep(w.ctx, w.s.Clock, d); err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

func (w *slowWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *slowWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
