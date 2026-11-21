// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin

package dataplane

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/testcp"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

type authFunc func(context.Context) (string, error)

func (f authFunc) NewDeviceSession(ctx context.Context) (string, error) { return f(ctx) }

// relayConn is one agent relay connection seen by the fake control plane.
type relayConn struct {
	t      *testing.T
	ctx    context.Context
	frames chan *ownsandv1.DataFrame
	stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]
}

func (rc *relayConn) send(f *ownsandv1.DataFrame) {
	rc.t.Helper()
	if err := rc.stream.Send(&ownsandv1.ConnectResponse{Frame: f}); err != nil {
		rc.t.Fatalf("relay send: %v", err)
	}
}

// next returns the next frame other than connection keepalives.
func (rc *relayConn) next() *ownsandv1.DataFrame {
	rc.t.Helper()
	for {
		select {
		case f := <-rc.frames:
			if f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE && !f.GetRelayKeepalive().GetEchoRequested() {
				continue
			}
			return f
		case <-time.After(20 * time.Second):
			rc.t.Fatal("no relay frame from the agent")
		}
	}
}

// nextOn returns the next frame of stream id, skipping exec keepalives.
func (rc *relayConn) nextOn(id uint64) *ownsandv1.DataFrame {
	rc.t.Helper()
	for {
		f := rc.next()
		if f.GetStreamId() == id && f.GetExecEvent().GetKeepalive() == nil {
			return f
		}
	}
}

type relayEnv struct {
	*world
	dp    *DataPlane
	cp    *testcp.Server
	conns chan *relayConn
	ch    *fakeChannel
	// beforeAuth, when set, runs before each device session credential
	// and fails the authentication with its error.
	beforeAuth atomic.Pointer[func(context.Context) error]
}

func newRelayEnv(t *testing.T, timing Timing, limits RelayLimits) *relayEnv {
	e := &relayEnv{}
	w := newWorld(t)
	cp := testcp.New(t)
	conns := make(chan *relayConn, 16)
	cp.SetRelayHandler(func(ctx context.Context, _ string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		rc := &relayConn{t: t, ctx: ctx, frames: make(chan *ownsandv1.DataFrame, 4096), stream: stream}
		conns <- rc
		for {
			req, err := stream.Receive()
			if err != nil {
				return nil
			}
			rc.frames <- req.GetFrame()
		}
	})
	hc, err := cpclient.NewHTTPClient(cp.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	dp := New(Options{
		Dispatcher: w.d, Sandboxes: w.m, Logger: logger(), Timing: timing, Limits: limits,
		Auth: authFunc(func(ctx context.Context) (string, error) {
			if hook := e.beforeAuth.Load(); hook != nil {
				if err := (*hook)(ctx); err != nil {
					return "", err
				}
			}
			return cp.IssueCredential("device"), nil
		}),
		HTTP:   hc,
		APIURL: cp.URL,
	})
	t.Cleanup(dp.Close)
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	e.world, e.dp, e.cp, e.conns, e.ch = w, dp, cp, conns, ch
	return e
}

func (e *relayEnv) conn() *relayConn {
	e.t.Helper()
	select {
	case rc := <-e.conns:
		if f := rc.next(); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS || len(f.GetSupportedFrameKinds().GetKinds()) == 0 {
			e.t.Fatalf("first frame after the keepalive %v", f)
		}
		return rc
	case <-time.After(10 * time.Second):
		e.t.Fatal("no relay connection")
	}
	return nil
}

func (e *relayEnv) grant(kind ownsandv1.RelayOperationKind, opID string, digest []byte, expiresIn, deadlineIn time.Duration, replaces string) (*ownsandv1.RelayGrant, *ownsandv1.RelayGrantAck) {
	g := &ownsandv1.RelayGrant{GrantId: newID(e.t), SandboxId: e.sandbox, OperationKind: kind, OperationId: opID, RequestDigest: digest,
		ExpiresIn: durationpb.New(expiresIn), StreamDeadlineIn: durationpb.New(deadlineIn)}
	if replaces != "" {
		g.ReplacesGrantId = &replaces
	}
	return g, e.dp.Grant(e.ch, g)
}

func (e *relayEnv) execGrant(req *ownsandv1.ExecRequest) *ownsandv1.RelayGrant {
	g, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.GetExecId(), relaybinding.ExecDigest(req), 15*time.Minute, 30*time.Minute, "")
	if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		e.t.Fatalf("grant ack %v", ack)
	}
	return g
}

func (e *relayEnv) open(rc *relayConn, id uint64, grantID string, window int64) {
	rc.send(datapath.MustNewFrame(id, e.sandbox, &ownsandv1.RelayStreamOpen{GrantId: grantID, InitialWindowBytes: window}))
}

func (e *relayEnv) openOK(rc *relayConn, id uint64, grantID string) {
	e.t.Helper()
	e.open(rc, id, grantID, 64<<20)
	if f := rc.nextOn(id); f.GetRelayWindowUpdate().GetIncrementBytes() < datapath.MinWindowSize {
		e.t.Fatalf("first window %v", f)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRelayGrantReleasedWhenDetachedExecEnds checks that the grant of an
// exec whose caller closed the stream is released when the exec ends, not
// only after the 60-second idle time.
func TestRelayGrantReleasedWhenDetachedExecEnds(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "sleep 1; echo done")
	g := e.execGrant(req)
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	if f := rc.nextOn(1); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
		t.Fatalf("expected the accepted notice, got %v", f)
	}
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamClose{}))
	time.Sleep(200 * time.Millisecond)
	if e.ch.released(g.GetGrantId()) {
		t.Fatal("the grant was released while the exec still ran")
	}
	waitFor(t, "grant release after the exec ended", func() bool { return e.ch.released(g.GetGrantId()) })
}

func TestRelayExecAndRelease(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	req := e.execReq("/bin/echo", "hi")
	g := e.execGrant(req)
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	if f := rc.nextOn(1); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
		t.Fatalf("expected the accepted notice, got %v", f)
	}
	var out string
	for {
		ev := rc.nextOn(1).GetExecEvent()
		out += string(ev.GetStdout().GetData())
		if ev.GetExited() != nil {
			break
		}
	}
	if out != "hi\n" {
		t.Fatalf("stdout %q", out)
	}
	waitFor(t, "grant release", func() bool { return e.ch.released(g.GetGrantId()) })

	// A stream open for no live grant.
	e.open(rc, 2, g.GetGrantId(), 64<<20)
	if f := rc.nextOn(2); f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("released grant %v", f)
	}
	// Echo.
	rc.send(keepaliveFrame(true))
	waitFor(t, "echo", func() bool {
		select {
		case f := <-rc.frames:
			return f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE
		default:
			return false
		}
	})
	// Unknown kinds: ignored on stream 0, an error frame elsewhere.
	rc.send(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind(99)})
	rc.send(&ownsandv1.DataFrame{StreamId: 3, SandboxId: e.sandbox, Kind: ownsandv1.DataFrameKind(99)})
	if f := rc.next(); f.GetStreamId() != 3 || f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED {
		t.Fatalf("unknown kind %v", f)
	}
}

func TestRelayGrantChecksCapsReplacementCloseAndRevocation(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{MaxStreamsPerGrant: 1, MaxGrants: 2})
	long := e.execReq("/bin/sh", "-c", "sleep 30")
	g1 := e.execGrant(long)
	rc := e.conn()
	e.openOK(rc, 1, g1.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, long))
	rc.nextOn(1) // accepted
	if f := rc.nextOn(1); f.GetExecEvent().GetStarted() == nil {
		t.Fatalf("started %v", f)
	}
	// The per-grant stream cap.
	e.open(rc, 2, g1.GetGrantId(), 64<<20)
	if f := rc.nextOn(2); f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("stream cap %v", f)
	}
	// A grant whose digest does not match the request.
	other := e.execReq("/bin/true")
	g2, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, other.GetExecId(), make([]byte, 32), 15*time.Minute, 30*time.Minute, "")
	if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		t.Fatal(ack)
	}
	// The per-device grant limit.
	if _, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, long.GetExecId(), nil, time.Minute, time.Minute, ""); ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT {
		t.Fatalf("grant limit %v", ack)
	}
	e.openOK(rc, 3, g2.GetGrantId())
	rc.send(datapath.MustNewFrame(3, e.sandbox, other))
	if f := rc.nextOn(3); f.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("digest mismatch %v", f)
	}
	waitFor(t, "release after a terminal error", func() bool { return e.ch.released(g2.GetGrantId()) })
	// Replacement takes the slot and ends the old grant's streams.
	g3, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, long.GetExecId(), relaybinding.ExecDigest(long), 15*time.Minute, 30*time.Minute, g1.GetGrantId())
	if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED || ack.GetReplacedGrantId() != g1.GetGrantId() {
		t.Fatalf("replacement ack %v", ack)
	}
	if f := rc.nextOn(1); f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_REPLACED {
		t.Fatalf("replaced stream %v", f)
	}
	// Relay stream close releases the output subscription; the exec runs on.
	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: long.GetExecId()}
	e.openOK(rc, 4, g3.GetGrantId())
	rc.send(datapath.MustNewFrame(4, e.sandbox, attach))
	rc.nextOn(4) // accepted
	rc.nextOn(4) // started
	rc.send(datapath.MustNewFrame(4, e.sandbox, &ownsandv1.RelayStreamClose{}))
	waitFor(t, "stream close", func() bool {
		e.dp.relay.mu.Lock()
		defer e.dp.relay.mu.Unlock()
		return len(e.dp.relay.conn.streams) == 0
	})
	if !e.d.HasRunningExec(e.sandbox) {
		t.Fatal("relay stream close stopped the exec")
	}
	// Revocation ends the grant's streams.
	e.openOK(rc, 5, g3.GetGrantId())
	rc.send(datapath.MustNewFrame(5, e.sandbox, attach))
	rc.nextOn(5)
	e.dp.Revoke(e.ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_RelayGrantId{RelayGrantId: g3.GetGrantId()}})
	for {
		f := rc.nextOn(5)
		if f.GetSessionEnd() != nil {
			if f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
				t.Fatalf("revocation %v", f)
			}
			break
		}
	}
}

func TestRelayWindows(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "head -c 400000 /dev/zero")
	g := e.execGrant(req)
	rc := e.conn()
	const window = 100000
	e.open(rc, 1, g.GetGrantId(), window)
	rc.nextOn(1) // window update
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	var used int64
	var got int
collect:
	for {
		select {
		case f := <-rc.frames:
			used += datapath.FrameCost(f)
			got += len(f.GetExecEvent().GetStdout().GetData())
			if f.GetExecEvent().GetExited() != nil {
				t.Fatal("exec finished beyond the window")
			}
		case <-time.After(time.Second):
			break collect
		}
	}
	if used > window || got == 0 {
		t.Fatalf("agent used %d bytes of credit (window %d), %d output bytes", used, window, got)
	}
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayWindowUpdate{IncrementBytes: 64 << 20}))
	for {
		ev := rc.nextOn(1).GetExecEvent()
		got += len(ev.GetStdout().GetData())
		if ev.GetExited() != nil {
			break
		}
	}
	if got != 400000 {
		t.Fatalf("output %d bytes", got)
	}
}

func TestRelayChannelLossAndNewCredential(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "sleep 30")
	g := e.execGrant(req)
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	rc.nextOn(1)
	rc.nextOn(1)
	e.dp.ChannelLost(e.ch)
	e.ch.Close()
	if f := rc.nextOn(1); f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST {
		t.Fatalf("channel loss %v", f)
	}
	select {
	case <-rc.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("relay connection kept after the channel loss")
	}
	// A new channel dials only after a new grant, with a new credential.
	e.ch = newChannel(2)
	e.dp.ChannelOpened(e.ch)
	select {
	case <-e.conns:
		t.Fatal("dialed without a grant")
	case <-time.After(300 * time.Millisecond):
	}
	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId()}
	g2, _ := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, req.GetExecId(), nil, 15*time.Minute, 30*time.Minute, "")
	rc2 := e.conn()
	e.openOK(rc2, 1, g2.GetGrantId())
	rc2.send(datapath.MustNewFrame(1, e.sandbox, attach))
	if f := rc2.nextOn(1); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
		t.Fatalf("attach on the new connection %v", f)
	}
	creds := e.cp.RelayCredentials()
	if len(creds) != 2 || creds[0] == creds[1] {
		t.Fatalf("relay credentials %d", len(creds))
	}
}

func TestRelayExpiryAndDeadline(t *testing.T) {
	e := newRelayEnv(t, Timing{AdmissionTolerance: 100 * time.Millisecond}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "sleep 30")
	expired, _ := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.GetExecId(), relaybinding.ExecDigest(req), 300*time.Millisecond, time.Hour, "")
	rc := e.conn()
	// Open within the expiry, but the first request after expiry plus
	// tolerance.
	e.openOK(rc, 1, expired.GetGrantId())
	time.Sleep(500 * time.Millisecond)
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	if f := rc.nextOn(1); f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("request after expiry %v", f)
	}
	// The stream deadline ends an admitted stream.
	g, _ := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.GetExecId(), relaybinding.ExecDigest(req), time.Hour, 1500*time.Millisecond, "")
	e.openOK(rc, 2, g.GetGrantId())
	rc.send(datapath.MustNewFrame(2, e.sandbox, req))
	for {
		f := rc.nextOn(2)
		if f.GetSessionEnd() != nil {
			if f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED {
				t.Fatalf("deadline %v", f)
			}
			break
		}
	}
}

func TestRelayReceiveTimeoutProbeAndIdle(t *testing.T) {
	e := newRelayEnv(t, Timing{ReceiveTimeout: 700 * time.Millisecond, KeepaliveInterval: time.Hour, ProbeIdle: time.Hour}, RelayLimits{})
	e.execGrant(e.execReq("/bin/true"))
	e.conn()
	// The fake sends nothing: the agent dials again after the receive
	// timeout.
	e.conn()

	p := newRelayEnv(t, Timing{ProbeIdle: 200 * time.Millisecond, ProbeTimeout: 300 * time.Millisecond}, RelayLimits{})
	p.execGrant(p.execReq("/bin/true"))
	rc := p.conn()
	time.Sleep(300 * time.Millisecond)
	p.execGrant(p.execReq("/bin/true"))
	if f := rc.next(); !f.GetRelayKeepalive().GetEchoRequested() {
		t.Fatalf("probe %v", f)
	}
	rc2 := p.conn() // unanswered probe: new connection
	time.Sleep(300 * time.Millisecond)
	p.execGrant(p.execReq("/bin/true"))
	if f := rc2.next(); !f.GetRelayKeepalive().GetEchoRequested() {
		t.Fatalf("probe %v", f)
	}
	rc2.send(keepaliveFrame(false))
	select {
	case <-p.conns:
		t.Fatal("answered probe replaced the connection")
	case <-time.After(time.Second):
	}

	idle := newRelayEnv(t, Timing{GrantIdle: 200 * time.Millisecond, ConnectionIdle: 500 * time.Millisecond}, RelayLimits{})
	g := idle.execGrant(idle.execReq("/bin/true"))
	rc3 := idle.conn()
	waitFor(t, "idle grant release", func() bool { return idle.ch.released(g.GetGrantId()) })
	select {
	case <-rc3.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("idle relay connection kept")
	}
}

// A grant of a new channel that arrives while the relay loop of the
// previous channel still authenticates gets a relay connection: the
// running loop dials for the new channel, whether the old attempt fails
// (and backs off) or succeeds after the channel changed.
func TestRelayLoopFollowsNewChannel(t *testing.T) {
	for _, authFails := range []bool{true, false} {
		t.Run(fmt.Sprintf("auth_fails=%v", authFails), func(t *testing.T) {
			e := newRelayEnv(t, Timing{ReconnectMax: 300 * time.Millisecond}, RelayLimits{})
			entered := make(chan struct{}, 1)
			gate := make(chan struct{})
			hook := func(ctx context.Context) error {
				select {
				case entered <- struct{}{}:
				default:
				}
				select {
				case <-gate:
				case <-ctx.Done():
					return ctx.Err()
				}
				if authFails {
					return errors.New("authentication failed")
				}
				return nil
			}
			e.beforeAuth.Store(&hook)
			e.execGrant(e.execReq("/bin/true"))
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("relay loop did not authenticate")
			}
			// The channel is replaced while the loop of the old channel
			// waits in its authentication.
			e.dp.ChannelLost(e.ch)
			e.ch.Close()
			e.ch = newChannel(2)
			e.dp.ChannelOpened(e.ch)
			g := e.execGrant(e.execReq("/bin/true"))
			e.beforeAuth.Store(nil)
			close(gate)
			rc := e.conn()
			e.openOK(rc, 1, g.GetGrantId())
		})
	}
}

// A relay endpoint that fails every connection at once gets attempts with
// back-off, not an unthrottled reconnect loop.
func TestRelayErrorKeepsBackoff(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: time.Second}, RelayLimits{})
	var attempts atomic.Int32
	e.cp.SetRelayHandler(func(context.Context, string, *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		attempts.Add(1)
		return connect.NewError(connect.CodeUnavailable, errors.New("relay unavailable"))
	})
	e.execGrant(e.execReq("/bin/true"))
	time.Sleep(1500 * time.Millisecond)
	// Back-off of 200, 400, 800 milliseconds allows about four attempts.
	if n := attempts.Load(); n < 1 || n > 6 {
		t.Fatalf("%d relay connection attempts in 1.5 seconds", n)
	}
}
