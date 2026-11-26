// SPDX-License-Identifier: Apache-2.0

package ownsand

// In-process fakes of the control plane and the device agent: Connect
// handlers from ownsandv1connect for the public services and the relay, and
// a pion answerer that serves the data channel. Each fake keeps only what
// the tests inspect.

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

const testAPIKey = "osk_test-secret-key-value"

// canonicalFakeID returns the lowercase text form of a valid UUID v7 and any
// other value unchanged. The real control plane canonicalizes sandbox and
// file operation IDs before it forwards requests and relay grants to an agent.
func canonicalFakeID(s string) string {
	if u, err := uuidv7.Parse(s); err == nil {
		return u.String()
	}
	return s
}

func mustID(t testing.TB) string {
	t.Helper()
	id, err := uuidv7.NewString()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func testTiming() timing {
	return timing{
		p2pOpenTimeout:       3 * time.Second,
		streamQueueTimeout:   200 * time.Millisecond,
		recoveryWindow:       1500 * time.Millisecond,
		streamStall:          400 * time.Millisecond,
		relayStreamLimit:     time.Second,
		writeCallLimit:       500 * time.Millisecond,
		streamMargin:         100 * time.Millisecond,
		grantIdle:            2 * time.Second,
		writeAttemptDeadline: 600 * time.Millisecond,
		backoffMin:           10 * time.Millisecond,
		backoffMax:           100 * time.Millisecond,
		pollInterval:         20 * time.Millisecond,
		createRetryWindow:    time.Second,
		callerDeadlineSkew:   250 * time.Millisecond,
	}
}

// loopbackSettingEngine restricts ICE to loopback host candidates, so the
// tests need no network and no STUN server. It selects candidates by their
// loopback IP address, not by the interface name: the loopback interface is
// "lo" on Linux and "lo0" on macOS, but on Windows Go reports its friendly
// name "Loopback Pseudo-Interface 1", which a name filter does not match.
func loopbackSettingEngine() *webrtc.SettingEngine {
	se := &webrtc.SettingEngine{}
	se.SetIncludeLoopbackCandidate(true)
	se.SetIPFilter(func(ip net.IP) bool { return ip.IsLoopback() })
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	return se
}

// relayAction scripts one relay call.
type relayAction struct {
	err              error         // fail before accepted
	abortNow         bool          // abort the connection before anything
	noAccept         bool          // end OK without accepted
	abortAfterAccept bool          // abort after accepted (and after the exec was registered)
	endAfter         int           // end OK after N output events (a rotation)
	abortAfter       int           // abort after N output events
	stallAfter       int           // stop sending after N output events until the caller gives up
	stallAccepted    bool          // send accepted, then nothing
	endAfterTime     time.Duration // end OK at the first event after this time
	delay            time.Duration
	// holdOpen keeps the stream open without any event (no accepted) until
	// the caller cancels it.
	holdOpen bool
	// keepalivesOnly sends relay keepalives every 10 ms without accepted
	// until the caller cancels the stream.
	keepalivesOnly bool
	// acceptedKeepalives sends accepted, then keepalives every 10 ms for
	// this long, and then serves the request as usual.
	acceptedKeepalives time.Duration
	// abortAfterKeepalives aborts the connection after the accepted
	// keepalives, without any data.
	abortAfterKeepalives bool
}

// p2pAction scripts one data channel request.
type p2pAction struct {
	endAfter  int // send a session-end frame after N output events
	reason    ownsandv1.SessionEndReason
	failAfter int // close the data channel without a session-end frame
	// endNow answers an exec request with a session-end frame before any
	// frame, so the session ends before the agent accepted the request.
	endNow bool
	// failNow closes the data channel at an exec request without any frame
	// and without a session-end frame.
	failNow bool
	// endAfterStarted sends a session-end frame right after the started
	// event of an exec, with no output: a quiet exec whose session expires
	// after the agent accepted the continuation.
	endAfterStarted bool
	// onRequest, when set, runs once the agent has the exec (started for an
	// exec request, looked up for an attach) and before it serves any
	// frame; the request then gets no frame (a file read request runs it as
	// soon as the agent has the request). A test uses it to end or retire the
	// session before the agent served the request.
	onRequest func()
	// afterStarted, when set, runs after the agent sent the started event
	// of an exec, so a test can end or retire the session after the agent
	// served the continuation.
	afterStarted func()
	// afterReadKeepalive, when set, answers a file read request with one
	// keepalive event and then runs the callback instead of serving the read:
	// the agent served the read, so a test can end or retire the session
	// after the agent served it and before any chunk arrives. The request
	// gets no further frame.
	afterReadKeepalive func()
	// raw, when set, answers the request with this frame only.
	raw func(streamID uint64, sandboxID string) *ownsandv1.DataFrame
}

// authAction scripts one AuthorizeRelay call.
type authAction struct {
	err             error
	abortAfterGrant bool
	missingGrantID  bool
	stall           bool // keep the connection open and never respond
}

type fakeGrant struct {
	id       string
	kind     ownsandv1.RelayOperationKind
	opID     string
	digest   []byte
	replaced bool
}

type fakeCP struct {
	t   testing.TB
	srv *httptest.Server

	mu          sync.Mutex
	orgID       string
	sandboxID   string
	state       ownsandv1.SandboxState
	endReason   ownsandv1.SandboxEndReason
	revoked     bool
	echoToken   bool
	redirectTo  string
	created     map[string]*ownsandv1.Sandbox
	createCalls int
	dropCreates int
	// stallCreates holds that many CreateSandbox requests open without a
	// response, after the dropped ones, until the caller gives up.
	stallCreates int
	createKeys   []string // idempotency_key of each CreateSandbox request
	// createErr, when set, returns the error of the n-th CreateSandbox
	// request (from 1), or nil to serve it.
	createErr func(n int) error

	expiresIn   time.Duration
	clockOffset time.Duration // added to expire_time: a server clock that differs from the caller's
	grants      map[string]*fakeGrant
	byAuthID    map[string]string
	authReqs    []*ownsandv1.AuthorizeRelayRequest
	authScript  func(n int, req *ownsandv1.AuthorizeRelayRequest) authAction
	relayCalls  map[string]int
	relayLog    []string
	relayExecs  []*ownsandv1.ExecRequest
	// heldEnded records when a held (holdOpen or keepalivesOnly) relay
	// stream ended because the caller cancelled it, and when it began.
	heldStarts  []time.Time
	heldEnds    []time.Time
	relayScript func(method string, n int) relayAction

	openSessions int
	iceFail      bool
	// iceGate and openGate hold every GetIceServers or OpenSession request
	// until the gate closes or the request context ends. The channel is
	// created by the test before the first request and never replaced.
	iceGate  chan struct{}
	openGate chan struct{}
	// gateErr, when set, is the error that a held request returns right
	// after it reaches its gate, in place of waiting for the gate.
	gateErr error
	// openErr, when set, is the error of every OpenSession request.
	openErr error
	// agentGate, when set, holds every peer-to-peer file read and write
	// chunk request at the agent until the gate closes or the data channel
	// closes. The agent reports "read" or "write" on agentHits (when set)
	// when a request reaches the gate. The channels are created by the test
	// before the first request and never replaced.
	agentGate chan struct{}
	agentHits chan string
	// iceServers are the ICE server URLs that GetIceServers returns.
	iceServers []string
	iceCalls   int // GetIceServers requests
	// gateHits receives one value for each request that reached a closed
	// gate (kind "ice" or "open"), so a test can wait for the stall.
	gateHits chan string
	// gateCanceled counts held requests whose context ended before the gate
	// opened (the SDK gave up on them).
	gateCanceled int
	p2pRequests  int
	p2pScript    func(n int) p2pAction
	answerers    []*webrtc.PeerConnection

	agent *fakeAgent
	auths []string // Authorization headers seen
	other []string // requests seen by nobody but the redirect target

	// heldEntered receives one value each time a held relay stream starts
	// (after the start is recorded), so a test can wait until the relay
	// is inside the hold. It has room for 8 values and hold never blocks on it.
	heldEntered chan struct{}
}

func newFakeCP(t testing.TB) *fakeCP {
	cp := &fakeCP{
		t:          t,
		orgID:      mustID(t),
		sandboxID:  mustID(t),
		state:      ownsandv1.SandboxState_SANDBOX_STATE_RUNNING,
		created:    map[string]*ownsandv1.Sandbox{},
		expiresIn:  30 * time.Second,
		grants:     map[string]*fakeGrant{},
		byAuthID:   map[string]string{},
		relayCalls: map[string]int{},
		agent:      newFakeAgent(),
	}
	cp.heldEntered = make(chan struct{}, 8)
	mux := http.NewServeMux()
	opts := connect.WithInterceptors(&fakeAuth{cp: cp})
	mux.Handle(ownsandv1connect.NewSandboxServiceHandler(&fakeSandboxes{cp: cp}, opts))
	mux.Handle(ownsandv1connect.NewDataPathServiceHandler(&fakeDataPath{cp: cp}, opts))
	mux.Handle(ownsandv1connect.NewRelayServiceHandler(&fakeRelay{cp: cp}, opts))
	mux.Handle(ownsandv1connect.NewOrganizationServiceHandler(&fakeOrgs{cp: cp}, opts))
	mux.Handle(ownsandv1connect.NewBillingServiceHandler(&fakeBilling{cp: cp}, opts))
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cp.mu.Lock()
		target := cp.redirectTo
		cp.auths = append(cp.auths, r.Header.Get("Authorization"))
		cp.mu.Unlock()
		if target != "" {
			http.Redirect(w, r, target+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.Config.Protocols = new(http.Protocols)
	srv.Config.Protocols.SetHTTP1(true)
	srv.Config.Protocols.SetUnencryptedHTTP2(true)
	srv.Start()
	cp.srv = srv
	t.Cleanup(func() {
		srv.CloseClientConnections()
		srv.Close()
		cp.mu.Lock()
		pcs := cp.answerers
		cp.mu.Unlock()
		for _, pc := range pcs {
			_ = pc.Close()
		}
	})
	return cp
}

func (cp *fakeCP) client(t testing.TB, tr Transport) *Client {
	t.Helper()
	c, err := New(Options{BaseURL: cp.srv.URL, APIKey: testAPIKey, OrganizationID: cp.orgID, Transport: tr, LookupEnv: func(string) (string, bool) { return "", false }})
	if err != nil {
		t.Fatal(err)
	}
	c.timing = testTiming()
	c.p2p.settingEngine = loopbackSettingEngine
	return c
}

func (cp *fakeCP) handle(t testing.TB, c *Client) *Sandbox {
	t.Helper()
	s, err := c.Sandbox("", cp.sandboxID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func (cp *fakeCP) setState(st ownsandv1.SandboxState) {
	cp.mu.Lock()
	cp.state = st
	cp.mu.Unlock()
	cp.agent.notifyAll()
}

func (cp *fakeCP) sandboxState() ownsandv1.SandboxState {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.state
}

func (cp *fakeCP) relayCount(method string) int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.relayCalls[method]
}

func (cp *fakeCP) authRequests() []*ownsandv1.AuthorizeRelayRequest {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return append([]*ownsandv1.AuthorizeRelayRequest(nil), cp.authReqs...)
}

func (cp *fakeCP) iceCallCount() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.iceCalls
}

func (cp *fakeCP) gateCanceledCount() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.gateCanceled
}

func (cp *fakeCP) openSessionCount() int {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	return cp.openSessions
}

func detailErr(code connect.Code, msg string, d *ownsandv1.ErrorDetail) error {
	err := connect.NewError(code, errors.New(msg))
	if d != nil {
		if ed, derr := connect.NewErrorDetail(d); derr == nil {
			err.AddDetail(ed)
		}
	}
	return err
}

func grantInvalid() error {
	return detailErr(connect.CodeUnavailable, "grant invalid", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, SessionEndReason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID})
}

// fakeAuth checks the bearer credential like the control plane.
type fakeAuth struct{ cp *fakeCP }

func (a *fakeAuth) check(h http.Header) error {
	a.cp.mu.Lock()
	revoked, echo := a.cp.revoked, a.cp.echoToken
	a.cp.mu.Unlock()
	got := h.Get("Authorization")
	if echo {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("rejected credential "+got))
	}
	if revoked || got != "Bearer "+testAPIKey {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credential"))
	}
	return nil
}

func (a *fakeAuth) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if err := a.check(req.Header()); err != nil {
			return nil, err
		}
		return next(ctx, req)
	}
}

func (a *fakeAuth) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (a *fakeAuth) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if err := a.check(conn.RequestHeader()); err != nil {
			return err
		}
		return next(ctx, conn)
	}
}

// fakeOrgs serves OrganizationService for the wrapper tests.
type fakeOrgs struct {
	ownsandv1connect.UnimplementedOrganizationServiceHandler
	cp *fakeCP
}

func (f *fakeOrgs) GetOrganization(_ context.Context, req *connect.Request[ownsandv1.GetOrganizationRequest]) (*connect.Response[ownsandv1.GetOrganizationResponse], error) {
	return connect.NewResponse(&ownsandv1.GetOrganizationResponse{Organization: &ownsandv1.Organization{OrganizationId: req.Msg.GetOrganizationId(), Name: "test"}}), nil
}

func (f *fakeOrgs) ListOrganizations(context.Context, *connect.Request[ownsandv1.ListOrganizationsRequest]) (*connect.Response[ownsandv1.ListOrganizationsResponse], error) {
	return connect.NewResponse(&ownsandv1.ListOrganizationsResponse{}), nil
}

// fakeSandboxes serves SandboxService.
type fakeSandboxes struct {
	ownsandv1connect.UnimplementedSandboxServiceHandler
	cp *fakeCP
}

func (f *fakeSandboxes) record() *ownsandv1.Sandbox {
	cp := f.cp
	sb := &ownsandv1.Sandbox{SandboxId: cp.sandboxID, OrganizationId: cp.orgID, State: cp.state}
	if IsTerminal(cp.state) {
		r := cp.endReason
		sb.EndReason = &r
	}
	return sb
}

func (f *fakeSandboxes) CreateSandbox(ctx context.Context, req *connect.Request[ownsandv1.CreateSandboxRequest]) (*connect.Response[ownsandv1.CreateSandboxResponse], error) {
	cp := f.cp
	cp.mu.Lock()
	cp.createCalls++
	key := req.Msg.GetIdempotencyKey()
	cp.createKeys = append(cp.createKeys, key)
	if cp.createErr != nil {
		if err := cp.createErr(cp.createCalls); err != nil {
			cp.mu.Unlock()
			return nil, err
		}
	}
	sb, ok := cp.created[key]
	if !ok {
		sb = &ownsandv1.Sandbox{SandboxId: mustID(cp.t), OrganizationId: req.Msg.GetOrganizationId(), State: ownsandv1.SandboxState_SANDBOX_STATE_CREATING}
		cp.created[key] = sb
	}
	drop := cp.dropCreates > 0
	if drop {
		cp.dropCreates--
	}
	cp.mu.Unlock()
	if drop {
		// The reservation exists but the response is lost.
		panic(http.ErrAbortHandler)
	}
	cp.mu.Lock()
	stall := cp.stallCreates > 0
	if stall {
		cp.stallCreates--
	}
	cp.mu.Unlock()
	if stall {
		// The response never comes; the request ends when the caller
		// closes the connection.
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return connect.NewResponse(&ownsandv1.CreateSandboxResponse{Sandbox: sb}), nil
}

func (f *fakeSandboxes) GetSandbox(_ context.Context, req *connect.Request[ownsandv1.GetSandboxRequest]) (*connect.Response[ownsandv1.GetSandboxResponse], error) {
	cp := f.cp
	cp.mu.Lock()
	defer cp.mu.Unlock()
	for _, sb := range cp.created {
		if sb.GetSandboxId() == canonicalFakeID(req.Msg.GetSandboxId()) {
			sb.State = ownsandv1.SandboxState_SANDBOX_STATE_RUNNING
			return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: sb}), nil
		}
	}
	if canonicalFakeID(req.Msg.GetSandboxId()) != cp.sandboxID {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	return connect.NewResponse(&ownsandv1.GetSandboxResponse{Sandbox: f.record()}), nil
}

func (f *fakeSandboxes) PauseSandbox(_ context.Context, _ *connect.Request[ownsandv1.PauseSandboxRequest]) (*connect.Response[ownsandv1.PauseSandboxResponse], error) {
	f.cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_PAUSED)
	f.cp.mu.Lock()
	defer f.cp.mu.Unlock()
	return connect.NewResponse(&ownsandv1.PauseSandboxResponse{Sandbox: f.record()}), nil
}

func (f *fakeSandboxes) ResumeSandbox(_ context.Context, _ *connect.Request[ownsandv1.ResumeSandboxRequest]) (*connect.Response[ownsandv1.ResumeSandboxResponse], error) {
	f.cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_RUNNING)
	f.cp.mu.Lock()
	defer f.cp.mu.Unlock()
	return connect.NewResponse(&ownsandv1.ResumeSandboxResponse{Sandbox: f.record()}), nil
}

func (f *fakeSandboxes) DeleteSandbox(_ context.Context, _ *connect.Request[ownsandv1.DeleteSandboxRequest]) (*connect.Response[ownsandv1.DeleteSandboxResponse], error) {
	f.cp.mu.Lock()
	f.cp.endReason = ownsandv1.SandboxEndReason_SANDBOX_END_REASON_DELETED_BY_CALLER
	f.cp.mu.Unlock()
	f.cp.setState(ownsandv1.SandboxState_SANDBOX_STATE_DELETED)
	f.cp.mu.Lock()
	defer f.cp.mu.Unlock()
	return connect.NewResponse(&ownsandv1.DeleteSandboxResponse{Sandbox: f.record()}), nil
}

// fakeDataPath serves DataPathService with a pion answerer.
type fakeDataPath struct {
	ownsandv1connect.UnimplementedDataPathServiceHandler
	cp *fakeCP
}

// hold blocks a request at a gate until the gate closes or the request ends.
// A nil gate never blocks.
func (f *fakeDataPath) hold(ctx context.Context, gate chan struct{}, kind string) error {
	if gate == nil {
		return nil
	}
	select {
	case <-gate:
		return nil
	default:
	}
	if f.cp.gateHits != nil {
		select {
		case f.cp.gateHits <- kind:
		default:
		}
	}
	if err := f.cp.gateErr; err != nil {
		return err
	}
	select {
	case <-gate:
		return nil
	case <-ctx.Done():
		f.cp.mu.Lock()
		f.cp.gateCanceled++
		f.cp.mu.Unlock()
		return ctx.Err()
	}
}

// holdAgent blocks a peer-to-peer file request of kind at the agent gate. It
// reports false when the data channel closed first, and then the request is
// not served. A nil gate never blocks.
func (cp *fakeCP) holdAgent(ctx context.Context, kind string) bool {
	gate := cp.agentGate
	if gate == nil {
		return true
	}
	select {
	case <-gate:
		return true
	default:
	}
	if cp.agentHits != nil {
		select {
		case cp.agentHits <- kind:
		default:
		}
	}
	select {
	case <-gate:
		return true
	case <-ctx.Done():
		return false
	}
}

func (f *fakeDataPath) GetIceServers(ctx context.Context, _ *connect.Request[ownsandv1.GetIceServersRequest]) (*connect.Response[ownsandv1.GetIceServersResponse], error) {
	cp := f.cp
	cp.mu.Lock()
	cp.iceCalls++
	gate := cp.iceGate
	urls := cp.iceServers
	cp.mu.Unlock()
	if err := f.hold(ctx, gate, "ice"); err != nil {
		return nil, err
	}
	res := &ownsandv1.GetIceServersResponse{}
	if len(urls) > 0 {
		res.IceServers = []*ownsandv1.IceServer{{Urls: urls}}
	}
	return connect.NewResponse(res), nil
}

func (f *fakeDataPath) OpenSession(ctx context.Context, req *connect.Request[ownsandv1.OpenSessionRequest]) (*connect.Response[ownsandv1.OpenSessionResponse], error) {
	cp := f.cp
	cp.mu.Lock()
	cp.openSessions++
	iceFail := cp.iceFail
	state := cp.state
	gate := cp.openGate
	openErr := cp.openErr
	cp.mu.Unlock()
	if err := f.hold(ctx, gate, "open"); err != nil {
		return nil, err
	}
	if openErr != nil {
		return nil, openErr
	}
	switch state {
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
	case ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("unreachable"))
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox not running"))
	}
	api := webrtc.NewAPI(webrtc.WithSettingEngine(*loopbackSettingEngine()))
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, err
	}
	cp.mu.Lock()
	cp.answerers = append(cp.answerers, pc)
	cp.mu.Unlock()
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != DataChannelLabel || !dc.Ordered() || dc.MaxRetransmits() != nil || dc.MaxPacketLifeTime() != nil || dc.Negotiated() {
			cp.t.Errorf("data channel %q does not follow the wire agreement", dc.Label())
			return
		}
		newFakeAgentSession(cp, pc, dc, canonicalFakeID(req.Msg.GetSandboxId()))
	})
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: req.Msg.GetSdpOffer()}); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	ans, err := pc.CreateAnswer(nil)
	if err != nil {
		return nil, err
	}
	gathered := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(ans); err != nil {
		return nil, err
	}
	select {
	case <-gathered:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	sdp := pc.LocalDescription().SDP
	if iceFail {
		// Close the answerer, so ICE never connects and the caller's data
		// channel does not open.
		_ = pc.Close()
	}
	return connect.NewResponse(&ownsandv1.OpenSessionResponse{
		SessionId:  mustID(cp.t),
		SdpAnswer:  sdp,
		ExpireTime: timestamppb.New(time.Now().Add(time.Hour)),
		ExpiresIn:  durationpb.New(time.Hour),
	}), nil
}

func (f *fakeDataPath) AuthorizeRelay(ctx context.Context, req *connect.Request[ownsandv1.AuthorizeRelayRequest]) (*connect.Response[ownsandv1.AuthorizeRelayResponse], error) {
	cp := f.cp
	cp.mu.Lock()
	cp.authReqs = append(cp.authReqs, proto.Clone(req.Msg).(*ownsandv1.AuthorizeRelayRequest))
	n := len(cp.authReqs)
	script := cp.authScript
	state := cp.state
	cp.mu.Unlock()
	var act authAction
	if script != nil {
		act = script(n, req.Msg)
	}
	if act.err != nil {
		return nil, act.err
	}
	if act.stall {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	switch state {
	case ownsandv1.SandboxState_SANDBOX_STATE_UNREACHABLE:
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("unreachable"))
	case ownsandv1.SandboxState_SANDBOX_STATE_RUNNING:
	case ownsandv1.SandboxState_SANDBOX_STATE_PAUSED:
		k := req.Msg.GetOperationKind()
		if k != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC && k != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("not running"))
		}
	default:
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox ended"))
	}
	cp.mu.Lock()
	id, ok := cp.byAuthID[req.Msg.GetAuthorizationId()]
	if !ok {
		id = mustID(cp.t)
		g := &fakeGrant{id: id, kind: req.Msg.GetOperationKind(), opID: canonicalFakeID(req.Msg.GetOperationId()), digest: req.Msg.GetRequestDigest()}
		if r := req.Msg.ReplacesGrantId; r != nil {
			if old := cp.grants[*r]; old != nil && old.opID == g.opID {
				old.replaced = true
			}
		}
		cp.grants[id] = g
		cp.byAuthID[req.Msg.GetAuthorizationId()] = id
	}
	exp, offset := cp.expiresIn, cp.clockOffset
	cp.mu.Unlock()
	if act.abortAfterGrant {
		panic(http.ErrAbortHandler)
	}
	if act.missingGrantID {
		id = ""
	}
	return connect.NewResponse(&ownsandv1.AuthorizeRelayResponse{GrantId: id, ExpireTime: timestamppb.New(time.Now().Add(exp + offset)), ExpiresIn: durationpb.New(exp)}), nil
}

// fakeRelay serves RelayService against the fake agent.
type fakeRelay struct {
	ownsandv1connect.UnimplementedRelayServiceHandler
	cp *fakeCP
}

func (f *fakeRelay) begin(method string, h http.Header, kinds ...ownsandv1.RelayOperationKind) (*fakeGrant, relayAction, error) {
	cp := f.cp
	cp.mu.Lock()
	cp.relayCalls[method]++
	n := cp.relayCalls[method]
	cp.relayLog = append(cp.relayLog, method)
	script := cp.relayScript
	g := cp.grants[h.Get(RelayGrantHeader)]
	replaced := g != nil && g.replaced
	cp.mu.Unlock()
	var act relayAction
	if script != nil {
		act = script(method, n)
	}
	if act.abortNow {
		panic(http.ErrAbortHandler)
	}
	if g == nil || replaced {
		return nil, act, grantInvalid()
	}
	ok := false
	for _, k := range kinds {
		ok = ok || g.kind == k
	}
	if !ok {
		return nil, act, connect.NewError(connect.CodePermissionDenied, errors.New("grant kind mismatch"))
	}
	if act.delay > 0 {
		time.Sleep(act.delay)
	}
	if act.err != nil {
		return nil, act, act.err
	}
	return g, act, nil
}

// relaySender applies the output-event actions of a relay call.
type relaySender struct {
	act     relayAction
	outputs int
	send    func(*ownsandv1.ExecEvent) error
	ctx     context.Context
	start   time.Time
}

var errRotate = errors.New("rotate")

func (r *relaySender) sendEvent(ev *ownsandv1.ExecEvent) error {
	if err := r.send(ev); err != nil {
		return err
	}
	if r.act.endAfterTime > 0 && time.Since(r.start) > r.act.endAfterTime {
		return errRotate
	}
	switch ev.GetEvent().(type) {
	case *ownsandv1.ExecEvent_Stdout, *ownsandv1.ExecEvent_Stderr, *ownsandv1.ExecEvent_OutputTruncated:
		r.outputs++
		if r.act.abortAfter > 0 && r.outputs >= r.act.abortAfter {
			panic(http.ErrAbortHandler)
		}
		if r.act.endAfter > 0 && r.outputs >= r.act.endAfter {
			return errRotate
		}
		if r.act.stallAfter > 0 && r.outputs >= r.act.stallAfter {
			<-r.ctx.Done()
			return r.ctx.Err()
		}
	}
	return nil
}

// hold serves the pre-acceptance hold actions. It reports whether it held
// the stream; the caller then returns the result.
func (f *fakeRelay) hold(ctx context.Context, act relayAction, keepalive func() error) (bool, error) {
	if !act.holdOpen && !act.keepalivesOnly {
		return false, nil
	}
	cp := f.cp
	cp.mu.Lock()
	cp.heldStarts = append(cp.heldStarts, time.Now())
	cp.mu.Unlock()
	select {
	case cp.heldEntered <- struct{}{}:
	default:
	}
	defer func() {
		cp.mu.Lock()
		cp.heldEnds = append(cp.heldEnds, time.Now())
		cp.mu.Unlock()
	}()
	if act.holdOpen {
		<-ctx.Done()
		return true, ctx.Err()
	}
	t := time.NewTicker(10 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return true, ctx.Err()
		case <-t.C:
			if err := keepalive(); err != nil {
				<-ctx.Done()
				return true, ctx.Err()
			}
		}
	}
}

// sendKeepalives sends keepalives for d, as an accepted stream that waits
// for its operation.
func sendKeepalives(ctx context.Context, d time.Duration, keepalive func() error) error {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if err := keepalive(); err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return nil
}

func (f *fakeRelay) execLike(ctx context.Context, method string, h http.Header, attach *ownsandv1.AttachExecRequest, execReq *ownsandv1.ExecRequest, send func(*ownsandv1.ExecEvent) error) error {
	g, act, err := f.begin(method, h, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC)
	if err != nil {
		return err
	}
	if held, herr := f.hold(ctx, act, func() error {
		return send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}})
	}); held {
		return herr
	}
	if act.noAccept {
		return nil
	}
	cp := f.cp
	var e *fakeExec
	var outOff, errOff int64
	if execReq != nil {
		if g.kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || g.opID != execReq.GetExecId() || !relaybinding.Equal(g.digest, relaybinding.ExecDigest(execReq)) {
			return connect.NewError(connect.CodePermissionDenied, errors.New("digest mismatch"))
		}
		if cp.sandboxState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("not running"))
		}
		if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}}); err != nil {
			return err
		}
		e = cp.agent.startExec(cp, execReq)
	} else {
		if g.opID != attach.GetExecId() {
			return connect.NewError(connect.CodePermissionDenied, errors.New("operation mismatch"))
		}
		e = cp.agent.lookup(attach.GetExecId())
		if e == nil {
			return connect.NewError(connect.CodeNotFound, errors.New("exec not found"))
		}
		if g.kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC && !relaybinding.Equal(g.digest, e.digest) {
			return connect.NewError(connect.CodePermissionDenied, errors.New("digest mismatch"))
		}
		if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}}); err != nil {
			return err
		}
		outOff, errOff = attach.GetStdoutOffset(), attach.GetStderrOffset()
	}
	if act.abortAfterAccept {
		panic(http.ErrAbortHandler)
	}
	if act.stallAccepted {
		<-ctx.Done()
		return ctx.Err()
	}
	if act.acceptedKeepalives > 0 {
		if err := sendKeepalives(ctx, act.acceptedKeepalives, func() error {
			return send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}})
		}); err != nil {
			return err
		}
		if act.abortAfterKeepalives {
			panic(http.ErrAbortHandler)
		}
	}
	rs := &relaySender{act: act, send: send, ctx: ctx, start: time.Now()}
	err = cp.agent.serveExec(ctx, cp, e, outOff, errOff, rs.sendEvent)
	if errors.Is(err, errRotate) {
		return nil
	}
	return err
}

func (f *fakeRelay) Exec(ctx context.Context, req *connect.Request[ownsandv1.ExecRequest], stream *connect.ServerStream[ownsandv1.ExecResponse]) error {
	// Capture the decoded payload before normalization or a scripted abort.
	cp := f.cp
	cp.mu.Lock()
	cp.relayExecs = append(cp.relayExecs, proto.Clone(req.Msg).(*ownsandv1.ExecRequest))
	cp.mu.Unlock()
	req.Msg.SandboxId = canonicalFakeID(req.Msg.GetSandboxId())
	return f.execLike(ctx, "Exec", req.Header(), nil, req.Msg, func(ev *ownsandv1.ExecEvent) error {
		return stream.Send(&ownsandv1.ExecResponse{Event: ev})
	})
}

func (f *fakeRelay) AttachExec(ctx context.Context, req *connect.Request[ownsandv1.AttachExecRequest], stream *connect.ServerStream[ownsandv1.AttachExecResponse]) error {
	req.Msg.SandboxId = canonicalFakeID(req.Msg.GetSandboxId())
	return f.execLike(ctx, "AttachExec", req.Header(), req.Msg, nil, func(ev *ownsandv1.ExecEvent) error {
		return stream.Send(&ownsandv1.AttachExecResponse{Event: ev})
	})
}

func (f *fakeRelay) ReadFile(ctx context.Context, req *connect.Request[ownsandv1.ReadFileRequest], stream *connect.ServerStream[ownsandv1.ReadFileResponse]) error {
	req.Msg.SandboxId = canonicalFakeID(req.Msg.GetSandboxId())
	req.Msg.ReadId = canonicalFakeID(req.Msg.GetReadId())
	g, act, err := f.begin("ReadFile", req.Header(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_READ_FILE)
	if err != nil {
		return err
	}
	if g.opID != req.Msg.GetReadId() || !relaybinding.Equal(g.digest, relaybinding.ReadFileDigest(req.Msg)) {
		return connect.NewError(connect.CodePermissionDenied, errors.New("digest mismatch"))
	}
	readKeepalive := func() error {
		return stream.Send(&ownsandv1.ReadFileResponse{Event: &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}}})
	}
	if held, herr := f.hold(ctx, act, readKeepalive); held {
		return herr
	}
	if act.noAccept {
		return nil
	}
	if err := stream.Send(&ownsandv1.ReadFileResponse{Event: &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Accepted{Accepted: &ownsandv1.RequestAccepted{}}}}); err != nil {
		return err
	}
	if act.stallAccepted {
		<-ctx.Done()
		return ctx.Err()
	}
	if act.acceptedKeepalives > 0 {
		if err := sendKeepalives(ctx, act.acceptedKeepalives, readKeepalive); err != nil {
			return err
		}
		if act.abortAfterKeepalives {
			panic(http.ErrAbortHandler)
		}
	}
	chunks := 0
	err = f.cp.agent.serveRead(req.Msg, func(ev *ownsandv1.FileReadEvent) error {
		if err := stream.Send(&ownsandv1.ReadFileResponse{Event: ev}); err != nil {
			return err
		}
		if ev.GetChunk() != nil {
			chunks++
			if act.abortAfter > 0 && chunks >= act.abortAfter {
				panic(http.ErrAbortHandler)
			}
			if act.endAfter > 0 && chunks >= act.endAfter {
				return errRotate
			}
		}
		return nil
	})
	if errors.Is(err, errRotate) {
		return nil
	}
	return err
}

func (f *fakeRelay) WriteFile(_ context.Context, req *connect.Request[ownsandv1.WriteFileRequest]) (*connect.Response[ownsandv1.WriteFileResponse], error) {
	req.Msg.SandboxId = canonicalFakeID(req.Msg.GetSandboxId())
	req.Msg.WriteId = canonicalFakeID(req.Msg.GetWriteId())
	g, act, err := f.begin("WriteFile", req.Header(), ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE)
	if err != nil {
		return nil, err
	}
	if g.opID != req.Msg.GetWriteId() {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("operation mismatch"))
	}
	res, err := f.cp.agent.write(req.Msg, g.digest)
	if err != nil {
		return nil, err
	}
	if act.abortAfter > 0 {
		// The agent applied the chunk, but the response is lost.
		panic(http.ErrAbortHandler)
	}
	return connect.NewResponse(res), nil
}

// fakeExec is one exec on the fake agent.
type fakeExec struct {
	id     string
	digest []byte

	mu      sync.Mutex
	changed chan struct{}
	out     [2][]byte
	base    [2]int64
	final   *ownsandv1.ExecEvent
	runs    int
}

func (e *fakeExec) notify() {
	close(e.changed)
	e.changed = make(chan struct{})
}

func (e *fakeExec) write(stream int, b []byte) {
	e.mu.Lock()
	e.out[stream] = append(e.out[stream], b...)
	e.notify()
	e.mu.Unlock()
}

func (e *fakeExec) finish(ev *ownsandv1.ExecEvent) {
	e.mu.Lock()
	e.final = ev
	e.notify()
	e.mu.Unlock()
}

// drop discards the first n kept bytes of a stream, like the agent's 8 MiB
// output limit.
func (e *fakeExec) drop(stream int, n int) {
	e.mu.Lock()
	e.out[stream] = e.out[stream][n:]
	e.base[stream] += int64(n)
	e.mu.Unlock()
}

func exitedEvent(code int64) *ownsandv1.ExecEvent {
	return &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{Status: &ownsandv1.ExecExited_ExitCode{ExitCode: code}}}}
}

type fakeAgent struct {
	mu        sync.Mutex
	execs     map[string]*fakeExec
	run       func(cmd []string, e *fakeExec)
	startTime *timestamppb.Timestamp
	execChunk int
	files     map[string][]byte
	reads     map[string][]byte
	writes    map[string]*fakeWrite
	readChunk int
	// endSizeDelta is added to the size that serveRead reports in the final
	// FileReadEnd event. Zero reports the real size.
	endSizeDelta int64
	watchers     []chan struct{}
	writeReqs    []*ownsandv1.WriteFileRequest
	readReqs     []*ownsandv1.ReadFileRequest
	execReqs     []*ownsandv1.ExecRequest
}

type fakeWrite struct {
	path   string
	digest []byte
	data   []byte
	done   *ownsandv1.WriteFileResponse
}

func newFakeAgent() *fakeAgent {
	return &fakeAgent{
		execs:     map[string]*fakeExec{},
		files:     map[string][]byte{},
		reads:     map[string][]byte{},
		writes:    map[string]*fakeWrite{},
		execChunk: datapath.MaxExecOutputChunkSize,
		readChunk: datapath.MaxFileChunkSize,
		run: func(cmd []string, e *fakeExec) {
			for _, a := range cmd[1:] {
				e.write(0, []byte(a+"\n"))
			}
			e.finish(exitedEvent(0))
		},
	}
}

func (a *fakeAgent) notifyAll() {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, e := range a.execs {
		e.mu.Lock()
		e.notify()
		e.mu.Unlock()
	}
}

func (a *fakeAgent) lookup(id string) *fakeExec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.execs[id]
}

func (a *fakeAgent) totalRuns() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := 0
	for _, e := range a.execs {
		e.mu.Lock()
		n += e.runs
		e.mu.Unlock()
	}
	return n
}

// startExec runs a new exec_id once; a known exec_id attaches from zero.
func (a *fakeAgent) startExec(_ *fakeCP, req *ownsandv1.ExecRequest) *fakeExec {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.execs[req.GetExecId()]; e != nil {
		return e
	}
	// Record only the first request seen for each exec_id so a recovery retry of
	// the same operation does not look like a second operation.
	a.execReqs = append(a.execReqs, proto.Clone(req).(*ownsandv1.ExecRequest))
	e := &fakeExec{id: req.GetExecId(), digest: relaybinding.ExecDigest(req), changed: make(chan struct{}), runs: 1}
	a.execs[e.id] = e
	run := a.run
	cmd := append([]string(nil), req.GetCommand()...)
	go run(cmd, e)
	return e
}

func (a *fakeAgent) serveExec(ctx context.Context, cp *fakeCP, e *fakeExec, outOff, errOff int64, send func(*ownsandv1.ExecEvent) error) error {
	if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Started{Started: &ownsandv1.ExecStarted{ExecId: e.id, StartTime: a.startTime}}}); err != nil {
		return err
	}
	off := [2]int64{outOff, errOff}
	for {
		e.mu.Lock()
		var pending []*ownsandv1.ExecEvent
		if off[0] < e.base[0] || off[1] < e.base[1] {
			off[0], off[1] = max(off[0], e.base[0]), max(off[1], e.base[1])
			pending = append(pending, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_OutputTruncated{OutputTruncated: &ownsandv1.ExecOutputTruncated{StdoutOffset: off[0], StderrOffset: off[1]}}})
		}
		for s := 0; s < 2; s++ {
			for off[s] < e.base[s]+int64(len(e.out[s])) {
				start := off[s] - e.base[s]
				end := min(start+int64(a.execChunk), int64(len(e.out[s])))
				out := &ownsandv1.ExecOutput{Offset: off[s], Data: append([]byte(nil), e.out[s][start:end]...)}
				if s == 0 {
					pending = append(pending, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: out}})
				} else {
					pending = append(pending, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stderr{Stderr: out}})
				}
				off[s] += end - start
			}
		}
		final := e.final
		ch := e.changed
		e.mu.Unlock()
		for _, ev := range pending {
			if err := send(ev); err != nil {
				return err
			}
		}
		if len(pending) > 0 {
			continue
		}
		if final != nil {
			return send(final)
		}
		if cp.sandboxState() == ownsandv1.SandboxState_SANDBOX_STATE_PAUSED {
			return send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Paused{Paused: &ownsandv1.ExecPaused{}}})
		}
		select {
		case <-ch:
		case <-time.After(fakeKeepalive):
			// The agent and the relay send keepalives while a command is
			// silent.
			if err := send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}}); err != nil {
				return err
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

const fakeKeepalive = 100 * time.Millisecond

// recordRead keeps a copy of a read request that reached the agent.
func (a *fakeAgent) recordRead(req *ownsandv1.ReadFileRequest) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.readReqs = append(a.readReqs, proto.Clone(req).(*ownsandv1.ReadFileRequest))
}

// readRequests returns the read requests that reached the agent, in order.
func (a *fakeAgent) readRequests() []*ownsandv1.ReadFileRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*ownsandv1.ReadFileRequest(nil), a.readReqs...)
}

func (a *fakeAgent) serveRead(req *ownsandv1.ReadFileRequest, send func(*ownsandv1.FileReadEvent) error) error {
	a.recordRead(req)
	a.mu.Lock()
	snap, ok := a.reads[req.GetReadId()]
	if !ok {
		src, exists := a.files[req.GetPath()]
		if !exists {
			a.mu.Unlock()
			return connect.NewError(connect.CodeNotFound, errors.New("no such file"))
		}
		snap = append([]byte(nil), src...)
		a.reads[req.GetReadId()] = snap
	}
	chunk := a.readChunk
	endSizeDelta := a.endSizeDelta
	a.mu.Unlock()
	for off := req.GetOffset(); off < int64(len(snap)); {
		end := min(off+int64(chunk), int64(len(snap)))
		if err := send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{Chunk: &ownsandv1.FileChunk{Offset: off, Data: snap[off:end]}}}); err != nil {
			return err
		}
		off = end
	}
	return send(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{End: &ownsandv1.FileReadEnd{SizeBytes: int64(len(snap)) + endSizeDelta}}})
}

func (a *fakeAgent) write(req *ownsandv1.WriteFileRequest, grantDigest []byte) (*ownsandv1.WriteFileResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.writeReqs = append(a.writeReqs, proto.Clone(req).(*ownsandv1.WriteFileRequest))
	w := a.writes[req.GetWriteId()]
	if w == nil {
		if req.GetPath() == "" {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("unknown write"))
		}
		w = &fakeWrite{path: req.GetPath(), digest: relaybinding.WriteFileDigest(req)}
		a.writes[req.GetWriteId()] = w
	}
	if grantDigest != nil && !relaybinding.Equal(grantDigest, w.digest) {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("digest mismatch"))
	}
	if w.done != nil {
		return w.done, nil
	}
	end := req.GetOffset() + int64(len(req.GetData()))
	if int64(len(w.data)) < end {
		w.data = append(w.data, make([]byte, end-int64(len(w.data)))...)
	}
	copy(w.data[req.GetOffset():], req.GetData())
	res := &ownsandv1.WriteFileResponse{WriteId: req.GetWriteId(), Offset: req.GetOffset(), Length: int64(len(req.GetData()))}
	if fin := req.GetFinish(); fin != nil {
		if fin.GetSizeBytes() != int64(len(w.data)) {
			return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("ranges do not cover the size"))
		}
		a.files[w.path] = append([]byte(nil), w.data...)
		res.Finished, res.SizeBytes = true, fin.GetSizeBytes()
		w.done = res
	}
	return res, nil
}

// fakeAgentSession serves one data channel.
type fakeAgentSession struct {
	cp        *fakeCP
	pc        *webrtc.PeerConnection
	dc        *webrtc.DataChannel
	sandboxID string
	dec       *datapath.Decoder
	sendMu    sync.Mutex
	ctx       context.Context
	cancel    context.CancelFunc
}

func newFakeAgentSession(cp *fakeCP, pc *webrtc.PeerConnection, dc *webrtc.DataChannel, sandboxID string) {
	ctx, cancel := context.WithCancel(context.Background())
	s := &fakeAgentSession{cp: cp, pc: pc, dc: dc, sandboxID: sandboxID, dec: datapath.NewDecoder(), ctx: ctx, cancel: cancel}
	dc.OnClose(cancel)
	dc.OnMessage(func(msg webrtc.DataChannelMessage) {
		frames, err := s.dec.Push(msg.Data)
		if err != nil {
			cp.t.Errorf("agent decoder: %v", err)
			return
		}
		for _, f := range frames {
			go s.serve(f)
		}
	})
}

func (s *fakeAgentSession) send(f *ownsandv1.DataFrame) error {
	msgs, err := datapath.EncodeMessages(f)
	if err != nil {
		return err
	}
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	for _, m := range msgs {
		if len(m) > datapath.MaxDataChannelMessageSize {
			s.cp.t.Errorf("message of %d bytes", len(m))
		}
		if err := s.dc.Send(m); err != nil {
			return err
		}
	}
	return nil
}

var errSessionGone = errors.New("session gone")

func (s *fakeAgentSession) serve(f *ownsandv1.DataFrame) {
	cp := s.cp
	if f.GetSandboxId() != s.sandboxID {
		cp.t.Errorf("frame for sandbox %s on a session for %s", f.GetSandboxId(), s.sandboxID)
		return
	}
	cp.mu.Lock()
	cp.p2pRequests++
	n := cp.p2pRequests
	script := cp.p2pScript
	cp.mu.Unlock()
	var act p2pAction
	if script != nil {
		act = script(n)
	}
	id := f.GetStreamId()
	frame := func(p proto.Message) *ownsandv1.DataFrame { return datapath.MustNewFrame(id, s.sandboxID, p) }
	errFrame := func(err error) {
		_ = s.send(frame(&ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode(connect.CodeOf(err)), Message: err.Error()}))
	}
	if act.raw != nil {
		_ = s.send(act.raw(id, s.sandboxID))
		return
	}
	outputs := 0
	sendExec := func(ev *ownsandv1.ExecEvent) error {
		if act.endNow || act.failNow {
			if act.endNow {
				_ = s.send(frame(&ownsandv1.SessionEnd{Reason: act.reason}))
				time.Sleep(20 * time.Millisecond)
			}
			_ = s.pc.Close()
			return errSessionGone
		}
		if err := s.send(frame(ev)); err != nil {
			return err
		}
		switch ev.GetEvent().(type) {
		case *ownsandv1.ExecEvent_Started:
			if act.afterStarted != nil {
				act.afterStarted()
			}
			if act.endAfterStarted {
				_ = s.send(frame(&ownsandv1.SessionEnd{Reason: act.reason}))
				time.Sleep(20 * time.Millisecond)
				_ = s.pc.Close()
				return errSessionGone
			}
		case *ownsandv1.ExecEvent_Stdout, *ownsandv1.ExecEvent_Stderr:
			outputs++
			if act.endAfter > 0 && outputs >= act.endAfter {
				_ = s.send(frame(&ownsandv1.SessionEnd{Reason: act.reason}))
				time.Sleep(20 * time.Millisecond)
				_ = s.pc.Close()
				return errSessionGone
			}
			if act.failAfter > 0 && outputs >= act.failAfter {
				_ = s.pc.Close()
				return errSessionGone
			}
		}
		return nil
	}
	switch f.GetKind() {
	case ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_REQUEST:
		req := f.GetExecRequest()
		if cp.sandboxState() != ownsandv1.SandboxState_SANDBOX_STATE_RUNNING {
			errFrame(connect.NewError(connect.CodeFailedPrecondition, errors.New("not running")))
			return
		}
		e := cp.agent.startExec(cp, req)
		if act.onRequest != nil {
			act.onRequest()
			return
		}
		if err := cp.agent.serveExec(s.ctx, cp, e, 0, 0, sendExec); err != nil && !errors.Is(err, errSessionGone) && s.ctx.Err() == nil {
			errFrame(err)
		}
	case ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST:
		req := f.GetExecAttachRequest()
		e := cp.agent.lookup(req.GetExecId())
		if e == nil {
			errFrame(connect.NewError(connect.CodeNotFound, errors.New("exec not found")))
			return
		}
		if act.onRequest != nil {
			act.onRequest()
			return
		}
		if err := cp.agent.serveExec(s.ctx, cp, e, req.GetStdoutOffset(), req.GetStderrOffset(), sendExec); err != nil && !errors.Is(err, errSessionGone) && s.ctx.Err() == nil {
			errFrame(err)
		}
	case ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_READ_REQUEST:
		if act.onRequest != nil {
			cp.agent.recordRead(f.GetFileReadRequest())
			act.onRequest()
			return
		}
		if act.afterReadKeepalive != nil {
			cp.agent.recordRead(f.GetFileReadRequest())
			_ = s.send(frame(&ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}}))
			act.afterReadKeepalive()
			return
		}
		if !cp.holdAgent(s.ctx, "read") {
			return
		}
		chunks := 0
		err := cp.agent.serveRead(f.GetFileReadRequest(), func(ev *ownsandv1.FileReadEvent) error {
			if err := s.send(frame(ev)); err != nil {
				return err
			}
			if ev.GetChunk() != nil {
				chunks++
				if act.endAfter > 0 && chunks >= act.endAfter {
					_ = s.send(frame(&ownsandv1.SessionEnd{Reason: act.reason}))
					time.Sleep(20 * time.Millisecond)
					_ = s.pc.Close()
					return errSessionGone
				}
				if act.failAfter > 0 && chunks >= act.failAfter {
					_ = s.pc.Close()
					return errSessionGone
				}
			}
			return nil
		})
		if err != nil && !errors.Is(err, errSessionGone) {
			errFrame(err)
		}
	case ownsandv1.DataFrameKind_DATA_FRAME_KIND_FILE_WRITE_CHUNK:
		if !cp.holdAgent(s.ctx, "write") {
			return
		}
		if act.failAfter > 0 {
			_ = s.pc.Close()
			return
		}
		res, err := cp.agent.write(f.GetFileWriteChunk(), nil)
		if err != nil {
			errFrame(err)
			return
		}
		_ = s.send(frame(res))
	}
}

func (a *fakeAgent) file(path string) ([]byte, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	b, ok := a.files[path]
	return b, ok
}

// setEndSizeDelta makes later reads report a final size that differs from the
// bytes that they sent by delta.
func (a *fakeAgent) setEndSizeDelta(delta int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.endSizeDelta = delta
}

func (a *fakeAgent) setFile(path string, b []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.files[path] = b
}

func (a *fakeAgent) execRequests() []*ownsandv1.ExecRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*ownsandv1.ExecRequest(nil), a.execReqs...)
}

func (a *fakeAgent) writeRequests() []*ownsandv1.WriteFileRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]*ownsandv1.WriteFileRequest(nil), a.writeReqs...)
}
