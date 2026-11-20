// SPDX-License-Identifier: FSL-1.1-ALv2

package app

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
)

func testRelayLimits() *relay.Limits {
	l := relay.Defaults()
	l.GrantAckTimeout = 400 * time.Millisecond
	l.SignalTimeout = 2 * time.Second
	l.ConnectionWait = 500 * time.Millisecond
	l.AcceptTimeout = 400 * time.Millisecond
	l.StreamKeepalive = 150 * time.Millisecond
	l.StreamRotation = 1200 * time.Millisecond
	l.RequestDeadline = time.Second
	l.WriteFileDeadline = time.Second
	l.ConnectionKeepalive = 200 * time.Millisecond
	l.ConnectionReceiveIdle = 3 * time.Second
	l.AuthorizePerSecond = 1000
	return &l
}

// dpEnv is an organization with a running sandbox on a connected fake agent.
type dpEnv struct {
	h       *harness
	f       fixture
	a       *fakeAgent
	sandbox string
	dp      ownsandv1connect.DataPathServiceClient
}

func newDPEnv(t *testing.T, limits *relay.Limits) *dpEnv {
	if limits == nil {
		limits = testRelayLimits()
	}
	h := newHarness(t, harnessOptions{opts: func(o *Options) { o.RelayLimits = limits }})
	f := h.newFixture()
	a := h.enrollAgent(f.org, f.ownerSub)
	h.setLimits(f, a.deviceID, &ownsandv1.DeviceLimits{VcpuCount: 8, MemoryMib: 16384, DiskGib: 100})
	a.connect(nil)
	resp, err := sandboxClient(h, f.apiKey).CreateSandbox(context.Background(), createReq(f.org, nil))
	must(t, err)
	a.command()
	a.report(resp.Msg.Sandbox.SandboxId, ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, 0)
	return &dpEnv{h: h, f: f, a: a, sandbox: resp.Msg.Sandbox.SandboxId, dp: ownsandv1connect.NewDataPathServiceClient(h.client, h.url, bearer(f.apiKey))}
}

func digest(b byte) []byte { s := sha256.Sum256([]byte{b}); return s[:] }

func (e *dpEnv) authorizeReq(kind ownsandv1.RelayOperationKind, opID string) *ownsandv1.AuthorizeRelayRequest {
	r := &ownsandv1.AuthorizeRelayRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, OperationKind: kind, OperationId: opID, AuthorizationId: datapath.NewID()}
	if kind != ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC {
		r.RequestDigest = digest(1)
	}
	return r
}

// authorize calls AuthorizeRelay and lets the agent answer with result.
func (e *dpEnv) authorize(dp ownsandv1connect.DataPathServiceClient, r *ownsandv1.AuthorizeRelayRequest, result ownsandv1.RelayGrantResult) (*ownsandv1.AuthorizeRelayResponse, *ownsandv1.RelayGrant, error) {
	type out struct {
		resp *ownsandv1.AuthorizeRelayResponse
		err  error
	}
	ch := make(chan out, 1)
	go func() {
		resp, err := dp.AuthorizeRelay(context.Background(), connect.NewRequest(r))
		if err != nil {
			ch <- out{err: err}
			return
		}
		ch <- out{resp: resp.Msg}
	}()
	var g *ownsandv1.RelayGrant
	if result != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_UNSPECIFIED {
		g = e.a.expect(func(env *ownsandv1.AgentEnvelope) bool { return env.GetRelayGrant() != nil }).GetRelayGrant()
		ack := &ownsandv1.RelayGrantAck{GrantId: g.GrantId, Result: result}
		if g.ReplacesGrantId != nil {
			ack.ReplacedGrantId = g.ReplacesGrantId
		}
		e.a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: ack}})
	}
	o := <-ch
	return o.resp, g, o.err
}

func retryDelay(t *testing.T, err error) time.Duration {
	t.Helper()
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeResourceExhausted {
		t.Fatalf("want resource_exhausted, got %v", err)
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if r, ok := v.(*ownsandv1.RetryDetail); ok {
				return r.RetryDelay.AsDuration()
			}
		}
	}
	t.Fatal("no retry delay in the error detail")
	return 0
}

func sessionEndReason(err error) ownsandv1.SessionEndReason {
	var ce *connect.Error
	if !errors.As(err, &ce) {
		return 0
	}
	for _, d := range ce.Details() {
		if v, e := d.Value(); e == nil {
			if r, ok := v.(*ownsandv1.SessionEndDetail); ok {
				return r.Reason
			}
		}
	}
	return 0
}

func TestSignalingAndSessionRevocation(t *testing.T) {
	e := newDPEnv(t, nil)
	ctx := context.Background()
	ice, err := e.dp.GetIceServers(ctx, connect.NewRequest(&ownsandv1.GetIceServersRequest{OrganizationId: e.f.org}))
	if err != nil || len(ice.Msg.IceServers) != 1 || ice.Msg.IceServers[0].Urls[0] != "stun:ownsand.api.delino.io:3478" {
		t.Fatalf("ice servers: %v %v", ice, err)
	}
	done := make(chan *ownsandv1.OpenSessionResponse, 1)
	go func() {
		resp, err := e.dp.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, SdpOffer: "v=0 offer"}))
		if err != nil {
			done <- nil
			return
		}
		done <- resp.Msg
	}()
	offer := e.a.expect(func(env *ownsandv1.AgentEnvelope) bool { return env.GetSignalingOffer() != nil }).GetSignalingOffer()
	if offer.SdpOffer != "v=0 offer" || offer.SandboxId != e.sandbox || offer.Principal.GetKind() != ownsandv1.PrincipalKind_PRINCIPAL_KIND_API_KEY || offer.ExpireTime == nil {
		t.Fatalf("offer: %v", offer)
	}
	e.a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_SignalingAnswer{SignalingAnswer: &ownsandv1.SignalingAnswer{SessionId: offer.SessionId, SdpAnswer: "v=0 answer"}}})
	resp := <-done
	if resp == nil || resp.SdpAnswer != "v=0 answer" || resp.SessionId != offer.SessionId {
		t.Fatalf("open session: %v", resp)
	}
	// Revoking the API key revokes its session on the agent.
	_, err = e.h.clients(userToken(e.f.ownerSub)).keys.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: e.f.org, ApiKeyId: e.f.apiKeyID}))
	must(t, err)
	rev := e.a.expect(func(env *ownsandv1.AgentEnvelope) bool { return env.GetSessionRevocation() != nil }).GetSessionRevocation()
	if rev.GetSessionId() != offer.SessionId || rev.RevocationId == "" {
		t.Fatalf("revocation: %v", rev)
	}
	e.a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RevocationAck{RevocationAck: &ownsandv1.RevocationAck{RevocationId: rev.RevocationId}}})
	// Paused or deleted sandboxes do not open sessions; SDP is never logged.
	if _, err := e.dp.OpenSession(ctx, connect.NewRequest(&ownsandv1.OpenSessionRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, SdpOffer: "x"})); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
	for _, s := range []string{"v=0 offer", "v=0 answer"} {
		if contains(e.h.logs.String(), s) {
			t.Fatal("SDP reached the logs")
		}
	}
}

func contains(s, sub string) bool {
	return len(sub) > 0 && len(s) >= len(sub) && (func() bool { return indexOf(s, sub) >= 0 })()
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestAuthorizeRelayGrants(t *testing.T) {
	limits := testRelayLimits()
	limits.GrantsPerDevice = 4
	limits.GrantsPerPrincipal = 3
	e := newDPEnv(t, limits)
	ctx := context.Background()
	kind := ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC

	req := e.authorizeReq(kind, datapath.NewID())
	resp, g, err := e.authorize(e.dp, req, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	if err != nil || resp.GrantId != g.GrantId || g.ExpiresIn.AsDuration() != 15*time.Minute || g.StreamDeadlineIn.AsDuration() != 30*time.Minute {
		t.Fatalf("authorize: %v %v %v", resp, g, err)
	}
	// A retry with the same authorization_id returns the same grant without a new one.
	again, _, err := e.authorize(e.dp, req, 0)
	if err != nil || again.GrantId != resp.GrantId {
		t.Fatalf("idempotent retry: %v %v", again, err)
	}
	// Two more grants reach the per-principal limit of 3.
	var grants []string
	grants = append(grants, resp.GrantId)
	for i := 0; i < 2; i++ {
		r, _, err := e.authorize(e.dp, e.authorizeReq(kind, datapath.NewID()), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
		must(t, err)
		grants = append(grants, r.GrantId)
	}
	_, _, err = e.authorize(e.dp, e.authorizeReq(kind, datapath.NewID()), 0)
	if retryDelay(t, err) <= 0 {
		t.Fatal("retry delay")
	}
	// Replacement at the limit uses no new slot and names the predecessor.
	rep := e.authorizeReq(kind, req.OperationId)
	rep.ReplacesGrantId = &resp.GrantId
	r2, g2, err := e.authorize(e.dp, rep, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	if err != nil || g2.GetReplacesGrantId() != resp.GrantId || r2.GrantId == resp.GrantId {
		t.Fatalf("replacement: %v %v", g2, err)
	}
	// Releasing grants frees slots: more sequential operations than the limit.
	release := func(id string) {
		e.a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantRelease{RelayGrantRelease: &ownsandv1.RelayGrantRelease{GrantId: id}}})
	}
	release(r2.GrantId)
	release(grants[1])
	release(grants[2])
	time.Sleep(100 * time.Millisecond)
	for i := 0; i < 10; i++ {
		r, _, err := e.authorize(e.dp, e.authorizeReq(kind, datapath.NewID()), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
		if err != nil {
			t.Fatalf("sequential operation %d: %v", i, err)
		}
		release(r.GrantId)
		time.Sleep(20 * time.Millisecond)
	}
	// The agent's own limit rejects a grant.
	_, _, err = e.authorize(e.dp, e.authorizeReq(kind, datapath.NewID()), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT)
	retryDelay(t, err)
	// No acknowledgement: unavailable; a late acknowledgement installs the
	// grant and a retry with the same authorization_id returns it.
	late := e.authorizeReq(kind, datapath.NewID())
	_, _, err = e.authorize(e.dp, late, 0)
	if code(err) != connect.CodeUnavailable {
		t.Fatalf("no ack: %v", err)
	}
	pending := e.a.expect(func(env *ownsandv1.AgentEnvelope) bool { return env.GetRelayGrant() != nil }).GetRelayGrant()
	e.a.send(&ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_RelayGrantAck{RelayGrantAck: &ownsandv1.RelayGrantAck{GrantId: pending.GrantId, Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED}}})
	time.Sleep(100 * time.Millisecond)
	got, _, err := e.authorize(e.dp, late, 0)
	if err != nil || got.GrantId != pending.GrantId {
		t.Fatalf("late ack: %v %v", got, err)
	}
	// Grants are revoked with their API key.
	_, err = e.h.clients(userToken(e.f.ownerSub)).keys.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: e.f.org, ApiKeyId: e.f.apiKeyID}))
	must(t, err)
	rev := e.a.expect(func(env *ownsandv1.AgentEnvelope) bool { return env.GetSessionRevocation() != nil }).GetSessionRevocation()
	if rev.GetGrantId() == "" {
		t.Fatalf("grant revocation: %v", rev)
	}
	// A stale replaces_grant_id after the agent channel is lost creates a separate grant.
	e2 := newDPEnv(t, nil)
	first, _, err := e2.authorize(e2.dp, e2.authorizeReq(kind, datapath.NewID()), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	must(t, err)
	e2.a.disconnect()
	e2.a.connect(&ownsandv1.StateReport{ReportId: 9, Sandboxes: []*ownsandv1.SandboxReport{{SandboxId: e2.sandbox, State: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING, Revision: &ownsandv1.SandboxRevision{Epoch: e2.a.epoch, Counter: e2.a.revision}}}})
	stale := e2.authorizeReq(kind, datapath.NewID())
	stale.ReplacesGrantId = &first.GrantId
	_, g3, err := e2.authorize(e2.dp, stale, ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	if err != nil || g3.ReplacesGrantId != nil {
		t.Fatalf("stale replacement: %v %v", g3, err)
	}
}

func TestAuthorizeRelayRateLimit(t *testing.T) {
	limits := testRelayLimits()
	limits.AuthorizePerSecond = 3
	e := newDPEnv(t, limits)
	var last error
	for i := 0; i < 5; i++ {
		_, last = e.dp.AuthorizeRelay(context.Background(), connect.NewRequest(&ownsandv1.AuthorizeRelayRequest{OrganizationId: e.f.org}))
	}
	retryDelay(t, last)
}

// relayAgent is the fake agent's relay connection.
type relayAgent struct {
	t      *testing.T
	stream *connect.BidiStreamForClient[ownsandv1.DataFrame, ownsandv1.DataFrame]
	in     chan *ownsandv1.DataFrame
	closed chan struct{}
	cancel context.CancelFunc
	mu     sync.Mutex
	err    error
}

func (a *fakeAgent) relayConnect(cred string) *relayAgent {
	a.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	// The relay connection is a connection of its own, like on a real agent.
	stream := ownsandv1connect.NewRelayAgentServiceClient(h2cClient(), a.h.url).Connect(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+cred)
	ra := &relayAgent{t: a.t, stream: stream, in: make(chan *ownsandv1.DataFrame, 256), closed: make(chan struct{}), cancel: cancel}
	a.t.Cleanup(cancel)
	ra.send(keepaliveFrame(false))
	ra.send(datapath.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_SupportedKinds{SupportedKinds: &ownsandv1.SupportedFrameKinds{Kinds: datapath.SupportedKinds()}}
	}))
	go func() {
		defer close(ra.closed)
		for {
			f, err := stream.Receive()
			if err != nil {
				ra.err = err
				return
			}
			select {
			case ra.in <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ra
}

func keepaliveFrame(echo bool) *ownsandv1.DataFrame {
	return datapath.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_Keepalive{Keepalive: &ownsandv1.FrameKeepalive{EchoRequested: echo}}
	})
}

func (ra *relayAgent) send(f *ownsandv1.DataFrame) {
	ra.mu.Lock()
	defer ra.mu.Unlock()
	_ = ra.stream.Send(f)
}

func (ra *relayAgent) expect(match func(*ownsandv1.DataFrame) bool) *ownsandv1.DataFrame {
	ra.t.Helper()
	timeout := time.After(5 * time.Second)
	for {
		select {
		case f := <-ra.in:
			if match(f) {
				return f
			}
		case <-ra.closed:
			ra.t.Fatalf("relay connection closed while waiting: %v", ra.err)
		case <-timeout:
			ra.t.Fatal("timed out waiting for a relay frame")
		}
	}
}

func (ra *relayAgent) waitClosed(d time.Duration) bool {
	select {
	case <-ra.closed:
		return true
	case <-time.After(d):
		return false
	}
}

func frame(id uint64, sandbox string, set func(*ownsandv1.DataFrame)) *ownsandv1.DataFrame {
	return datapath.NewFrame(id, sandbox, set)
}

func relayClient(h *harness, cred, grantID string) ownsandv1connect.RelayServiceClient {
	return ownsandv1connect.NewRelayServiceClient(h.client, h.url, connect.WithInterceptors(relayHeaders{cred: cred, grant: grantID}))
}

// relayHeaders sets the credential and the grant ID on unary and streaming calls.
type relayHeaders struct{ cred, grant string }

func (r relayHeaders) set(h interface{ Set(string, string) }) {
	h.Set("Authorization", "Bearer "+r.cred)
	h.Set(datapath.RelayGrantHeader, r.grant)
}

func (r relayHeaders) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		r.set(req.Header())
		return next(ctx, req)
	}
}

func (r relayHeaders) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return func(ctx context.Context, spec connect.Spec) connect.StreamingClientConn {
		c := next(ctx, spec)
		r.set(c.RequestHeader())
		return c
	}
}

func (r relayHeaders) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

func execReq(e *dpEnv, execID string) *ownsandv1.ExecRequest {
	return &ownsandv1.ExecRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, ExecId: execID, Command: []string{"/bin/echo", "secret-arg"}, BindingSalt: make([]byte, 32)}
}

func TestRelayExecForwarding(t *testing.T) {
	e := newDPEnv(t, nil)
	ctx := context.Background()
	ra := e.a.relayConnect(e.a.credential())
	kinds := ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetSupportedKinds() != nil })
	if len(kinds.GetSupportedKinds().Kinds) == 0 {
		t.Fatal("no supported kinds")
	}
	// Echo reply.
	ra.send(keepaliveFrame(true))
	ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetKeepalive() != nil })

	execID := datapath.NewID()
	resp, _, err := e.authorize(e.dp, e.authorizeReq(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, execID), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	must(t, err)
	rc := relayClient(e.h, e.f.apiKey, resp.GrantId)
	events := make(chan *ownsandv1.ExecEvent, 64)
	errc := make(chan error, 1)
	go func() {
		s, err := rc.Exec(ctx, connect.NewRequest(execReq(e, execID)))
		if err != nil {
			errc <- err
			return
		}
		for s.Receive() {
			events <- s.Msg()
		}
		errc <- s.Err()
	}()
	open := ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
	if open.StreamId != 1 || open.GetRelayStreamOpen().GrantId != resp.GrantId || open.GetRelayStreamOpen().InitialWindow < datapath.MaxFrameSize {
		t.Fatalf("stream open: %v", open)
	}
	sid := open.StreamId
	ra.send(frame(sid, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: 4 << 20}}
	}))
	reqFrame := ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetExecRequest() != nil })
	if reqFrame.GetExecRequest().ExecId != execID || reqFrame.GetExecRequest().Command[1] != "secret-arg" {
		t.Fatalf("exec request: %v", reqFrame)
	}
	ra.send(frame(sid, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RequestAccepted{RequestAccepted: &ownsandv1.RequestAccepted{}}
	}))
	for _, ev := range []*ownsandv1.ExecEvent{
		{Event: &ownsandv1.ExecEvent_Started_{Started: &ownsandv1.ExecEvent_Started{ExecId: execID}}},
		{Event: &ownsandv1.ExecEvent_Output_{Output: &ownsandv1.ExecEvent_Output{Stream: ownsandv1.OutputStream_OUTPUT_STREAM_STDOUT, Data: []byte("secret-output")}}},
		{Event: &ownsandv1.ExecEvent_Exited_{Exited: &ownsandv1.ExecEvent_Exited{Result: &ownsandv1.ExecEvent_Exited_ExitCode{ExitCode: 0}}}},
	} {
		ra.send(frame(sid, e.sandbox, func(f *ownsandv1.DataFrame) { f.Payload = &ownsandv1.DataFrame_ExecEvent{ExecEvent: ev} }))
	}
	if err := <-errc; err != nil {
		t.Fatalf("exec: %v", err)
	}
	var got []string
	for len(events) > 0 {
		ev := <-events
		got = append(got, string(ev.ProtoReflect().WhichOneof(ev.ProtoReflect().Descriptor().Oneofs().ByName("event")).Name()))
	}
	want := []string{"accepted", "started", "output", "exited"}
	if len(got) != len(want) {
		t.Fatalf("events %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events %v", got)
		}
	}
	ra.expect(func(f *ownsandv1.DataFrame) bool { return f.StreamId == sid && f.GetRelayWindowUpdate() != nil })
	ra.expect(func(f *ownsandv1.DataFrame) bool { return f.StreamId == sid && f.GetRelayStreamClose() != nil })

	// A second RPC gets a new stream ID; a session-end frame ends it with unavailable.
	go func() {
		s, err := rc.AttachExec(ctx, connect.NewRequest(&ownsandv1.AttachExecRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, ExecId: execID}))
		if err != nil {
			errc <- err
			return
		}
		for s.Receive() {
		}
		errc <- s.Err()
	}()
	open2 := ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
	if open2.StreamId != 2 {
		t.Fatalf("stream id reuse: %d", open2.StreamId)
	}
	ra.send(frame(open2.StreamId, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_SessionEnd{SessionEnd: &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED}}
	}))
	err = <-errc
	if code(err) != connect.CodeUnavailable || sessionEndReason(err) != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
		t.Fatalf("session end: %v", err)
	}
	// Forwarding never logs content.
	logs := e.h.logs.String()
	if contains(logs, "secret-arg") || contains(logs, "secret-output") {
		t.Fatal("relay content reached the logs")
	}
	if !contains(logs, `"relay_rpc"`) {
		t.Fatal("no relay RPC log record")
	}
}

func TestRelayAuthenticationFailures(t *testing.T) {
	e := newDPEnv(t, nil)
	ctx := context.Background()
	_ = e.a.relayConnect(e.a.credential())
	execID := datapath.NewID()
	resp, _, err := e.authorize(e.dp, e.authorizeReq(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, execID), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	must(t, err)
	call := func(cred, grantID string, req *ownsandv1.ExecRequest) error {
		s, err := relayClient(e.h, cred, grantID).Exec(ctx, connect.NewRequest(req))
		if err != nil {
			return err
		}
		for s.Receive() {
		}
		return s.Err()
	}
	if err := call(e.f.apiKey, datapath.NewID(), execReq(e, execID)); code(err) != connect.CodeUnavailable || sessionEndReason(err) != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("unknown grant: %v", err)
	}
	if err := call(userToken(e.f.memberSub), resp.GrantId, execReq(e, execID)); code(err) != connect.CodePermissionDenied {
		t.Fatalf("other principal: %v", err)
	}
	other := execReq(e, datapath.NewID())
	if err := call(e.f.apiKey, resp.GrantId, other); code(err) != connect.CodePermissionDenied {
		t.Fatalf("other operation: %v", err)
	}
	bad := execReq(e, execID)
	bad.SandboxId = datapath.NewID()
	if err := call(e.f.apiKey, resp.GrantId, bad); code(err) != connect.CodePermissionDenied {
		t.Fatalf("other sandbox: %v", err)
	}
	// ReadFile with an EXEC grant does not match the operation kind.
	s, err := relayClient(e.h, e.f.apiKey, resp.GrantId).ReadFile(ctx, connect.NewRequest(&ownsandv1.ReadFileRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, ReadId: execID}))
	if err == nil {
		for s.Receive() {
		}
		err = s.Err()
	}
	if code(err) != connect.CodePermissionDenied {
		t.Fatalf("kind mismatch: %v", err)
	}
	// A user credential cannot open a relay connection.
	us := ownsandv1connect.NewRelayAgentServiceClient(e.h.client, e.h.url).Connect(ctx)
	us.RequestHeader().Set("Authorization", "Bearer "+userToken(e.f.ownerSub))
	_ = us.Send(keepaliveFrame(false))
	if _, err := us.Receive(); code(err) != connect.CodePermissionDenied {
		t.Fatalf("user on Connect: %v", err)
	}
	// After the API key is revoked, its relay RPCs are unauthenticated.
	_, err = e.h.clients(userToken(e.f.ownerSub)).keys.RevokeApiKey(ctx, connect.NewRequest(&ownsandv1.RevokeApiKeyRequest{OrganizationId: e.f.org, ApiKeyId: e.f.apiKeyID}))
	must(t, err)
	if err := call(e.f.apiKey, resp.GrantId, execReq(e, execID)); code(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked key: %v", err)
	}
}

func TestRelayConnectionRulesAndTimers(t *testing.T) {
	e := newDPEnv(t, nil)
	ctx := context.Background()
	execID := datapath.NewID()
	resp, _, err := e.authorize(e.dp, e.authorizeReq(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, execID), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	must(t, err)
	rc := relayClient(e.h, e.f.apiKey, resp.GrantId)
	var keepalives atomic.Int64
	run := func() chan error {
		errc := make(chan error, 1)
		go func() {
			s, err := rc.Exec(ctx, connect.NewRequest(execReq(e, execID)))
			if err != nil {
				errc <- err
				return
			}
			n := 0
			for s.Receive() {
				if s.Msg().GetKeepalive() != nil {
					keepalives.Add(1)
				}
				n++
			}
			errc <- s.Err()
		}()
		return errc
	}
	// No relay connection within the wait.
	if err := <-run(); code(err) != connect.CodeUnavailable {
		t.Fatalf("no relay connection: %v", err)
	}
	ra := e.a.relayConnect(e.a.credential())
	accept := func(window int64) uint64 {
		open := ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
		ra.send(frame(open.StreamId, e.sandbox, func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: window}}
		}))
		ra.expect(func(f *ownsandv1.DataFrame) bool { return f.GetExecRequest() != nil })
		return open.StreamId
	}
	// Acceptance timeout: unavailable and a stream close.
	errc := run()
	sid := accept(4 << 20)
	if err := <-errc; code(err) != connect.CodeUnavailable {
		t.Fatalf("acceptance timeout: %v", err)
	}
	ra.expect(func(f *ownsandv1.DataFrame) bool { return f.StreamId == sid && f.GetRelayStreamClose() != nil })
	// Keepalives, then rotation with OK and no final event.
	errc = run()
	sid = accept(4 << 20)
	ra.send(frame(sid, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RequestAccepted{RequestAccepted: &ownsandv1.RequestAccepted{}}
	}))
	start := time.Now()
	if err := <-errc; err != nil {
		t.Fatalf("rotation: %v", err)
	}
	if time.Since(start) < 500*time.Millisecond {
		t.Fatal("rotated too early")
	}
	if keepalives.Load() < 2 {
		t.Fatalf("keepalive events: %d", keepalives.Load())
	}
	ra.expect(func(f *ownsandv1.DataFrame) bool { return f.StreamId == sid && f.GetRelayStreamClose() != nil })
	// An agent that sends beyond its credit window loses the connection.
	errc = run()
	sid = accept(4 << 20)
	big := make([]byte, 1<<20)
	for i := 0; i < 3; i++ {
		ra.send(frame(sid, e.sandbox, func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_ExecEvent{ExecEvent: &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Output_{Output: &ownsandv1.ExecEvent_Output{Data: big}}}}
		}))
	}
	if err := <-errc; code(err) != connect.CodeUnavailable {
		t.Fatalf("credit violation: %v", err)
	}
	if !ra.waitClosed(3 * time.Second) {
		t.Fatal("connection stayed open after a credit violation")
	}
	// Replacement: a second connection ends the RPCs of the first.
	r1 := e.a.relayConnect(e.a.credential())
	errc = run()
	open := r1.expect(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
	_ = open
	r2 := e.a.relayConnect(e.a.credential())
	if err := <-errc; code(err) != connect.CodeUnavailable {
		t.Fatalf("replaced connection RPC: %v", err)
	}
	if !r1.waitClosed(3 * time.Second) {
		t.Fatal("replaced connection stayed open")
	}
	// WriteFile round trip.
	writeID := datapath.NewID()
	wres, _, err := e.authorize(e.dp, e.authorizeReq(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID), ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED)
	must(t, err)
	wc := relayClient(e.h, e.f.apiKey, wres.GrantId)
	wdone := make(chan error, 1)
	var wresp *ownsandv1.WriteFileResponse
	go func() {
		r, err := wc.WriteFile(ctx, connect.NewRequest(&ownsandv1.WriteFileRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, WriteId: writeID, Path: "/tmp/x", Chunk: &ownsandv1.FileChunk{Data: []byte("hello")}}))
		if err == nil {
			wresp = r.Msg
		}
		wdone <- err
	}()
	wopen := r2.expect(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
	r2.send(frame(wopen.StreamId, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: 4 << 20}}
	}))
	r2.expect(func(f *ownsandv1.DataFrame) bool { return f.GetFileWriteChunk() != nil })
	r2.send(frame(wopen.StreamId, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_FileWriteResult{FileWriteResult: &ownsandv1.WriteFileResponse{WrittenBytes: 5}}
	}))
	if err := <-wdone; err != nil || wresp.WrittenBytes != 5 {
		t.Fatalf("write: %v %v", wresp, err)
	}
	// The WriteFile deadline covers the agent's result.
	go func() {
		_, err := wc.WriteFile(ctx, connect.NewRequest(&ownsandv1.WriteFileRequest{OrganizationId: e.f.org, SandboxId: e.sandbox, WriteId: writeID, Chunk: &ownsandv1.FileChunk{Offset: 5}}))
		wdone <- err
	}()
	wopen = r2.expect(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil })
	r2.send(frame(wopen.StreamId, e.sandbox, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: 4 << 20}}
	}))
	if err := <-wdone; code(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("write deadline: %v", err)
	}
	r2.expect(func(f *ownsandv1.DataFrame) bool {
		return f.StreamId == wopen.StreamId && f.GetRelayStreamClose() != nil
	})
	// Closing the agent channel closes the relay connection.
	e.a.disconnect()
	if !r2.waitClosed(3 * time.Second) {
		t.Fatal("relay connection outlived its agent channel")
	}
}
