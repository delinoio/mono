// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
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
	return newRelayEnvWrapped(t, timing, limits, nil)
}

// newRelayEnvWrapped is newRelayEnv with wrap applied to the HTTP transport
// that the relay client uses, so a test can observe its requests.
func newRelayEnvWrapped(t *testing.T, timing Timing, limits RelayLimits, wrap func(http.RoundTripper) http.RoundTripper) *relayEnv {
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
	if wrap != nil {
		hc.Transport = wrap(hc.Transport)
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

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// gatedBody is a response body whose Read blocks until release is closed and
// then returns data, so a test completes a read at a chosen moment.
type gatedBody struct {
	started chan struct{}
	release chan struct{}
	data    []byte
	once    sync.Once
}

func newGatedBody(data string) *gatedBody {
	return &gatedBody{started: make(chan struct{}), release: make(chan struct{}), data: []byte(data)}
}

func (b *gatedBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return copy(p, b.data), nil
}

func (b *gatedBody) Close() error { return nil }

// TestRelayLateReadOfRetiredResponseKeepsReplacementClock checks that the
// receive clock follows the connection that issued the request: a read of
// the retired connection's response that completes after the replacement
// connection was installed refreshes the retired connection only, so the
// replacement's receive timeout and echo probe keep their own bytes. A read
// of the active response, also a partial frame, still refreshes its own clock.
func TestRelayLateReadOfRetiredResponseKeepsReplacementClock(t *testing.T) {
	// A fixed monotonic clock makes the refresh visible and deterministic.
	const readAt = 2 * time.Hour
	clock := func() time.Duration { return readAt }
	retired, active := &rconn{clock: clock}, &rconn{clock: clock}
	r := &relay{conn: retired}
	bodies := map[*rconn]*gatedBody{retired: newGatedBody("late"), active: newGatedBody("par")}
	hc := &http.Client{Transport: countingTransport{base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		c, _ := req.Context().Value(connCtxKey{}).(*rconn)
		return &http.Response{StatusCode: http.StatusOK, Body: bodies[c], Request: req}, nil
	})}}
	open := func(c *rconn) io.ReadCloser {
		req, err := http.NewRequestWithContext(withConn(context.Background(), c), http.MethodPost, "http://relay.test/connect", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.Body
	}
	retiredBody := open(retired)
	read := func(body io.ReadCloser) <-chan struct{} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			if _, err := body.Read(make([]byte, 16)); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
		return done
	}
	// The retired response has a read in flight while the loop installs the
	// replacement connection.
	lateRead := read(retiredBody)
	<-bodies[retired].started
	r.mu.Lock()
	r.conn = active
	r.mu.Unlock()
	activeBody := open(active)

	old := int64(time.Hour)
	retired.lastRecv.Store(old)
	active.lastRecv.Store(old)
	close(bodies[retired].release)
	<-lateRead
	if got := active.lastRecv.Load(); got != old {
		t.Fatalf("a late read of the retired response moved the replacement's receive clock by %v", time.Duration(got-old))
	}
	if retired.lastRecv.Load() == old {
		t.Fatal("a read did not refresh the receive clock of the connection that issued the request")
	}

	// A partial frame of the active response refreshes the active clock
	// before any frame is decoded.
	activeRead := read(activeBody)
	<-bodies[active].started
	if active.lastRecv.Load() != old {
		t.Fatal("the receive clock moved before any byte arrived")
	}
	close(bodies[active].release)
	<-activeRead
	if active.lastRecv.Load() == old {
		t.Fatal("a read of the active response did not refresh its receive clock")
	}
}

// TestRelayConnectBindsResponseBodyToConnection checks that connectOnce binds
// the Connect request to the connection that it installs, so the byte
// counting of its response reaches that connection.
func TestRelayConnectBindsResponseBodyToConnection(t *testing.T) {
	var bound atomic.Pointer[rconn]
	e := newRelayEnvWrapped(t, Timing{}, RelayLimits{}, func(base http.RoundTripper) http.RoundTripper {
		return roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if c, _ := req.Context().Value(connCtxKey{}).(*rconn); c != nil {
				bound.Store(c)
			}
			return base.RoundTrip(req)
		})
	})
	e.execGrant(e.execReq("/bin/true"))
	e.conn()
	e.dp.relay.mu.Lock()
	installed := e.dp.relay.conn
	e.dp.relay.mu.Unlock()
	if installed == nil || bound.Load() != installed {
		t.Fatalf("the Connect request is bound to %p, the installed connection is %p", bound.Load(), installed)
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

// TestRelayEmptyStreamKeepsAttachGrantOfEndedExec checks that a stream that
// closes before it carries a request does not mark an ATTACH_EXEC grant as
// used: the grant of an exec that already ended stays until its idle time, a
// second stream attaches under it and gets the retained exit event, and the
// grant is released only after that attach ended.
func TestRelayEmptyStreamKeepsAttachGrantOfEndedExec(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "echo done")
	g := e.execGrant(req)
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamClose{}))
	waitFor(t, "exec end", func() bool { return e.d.ExecEnded(req.GetExecId()) })
	waitFor(t, "release of the exec grant", func() bool { return e.ch.released(g.GetGrantId()) })

	attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId()}
	g2, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, req.GetExecId(), nil, 15*time.Minute, 30*time.Minute, "")
	if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		t.Fatalf("grant ack %v", ack)
	}
	e.openOK(rc, 2, g2.GetGrantId())
	rc.send(datapath.MustNewFrame(2, e.sandbox, &ownsandv1.RelayStreamClose{}))
	waitFor(t, "empty stream end", func() bool {
		e.dp.relay.mu.Lock()
		defer e.dp.relay.mu.Unlock()
		// A released grant is gone from the map; the checks below report it.
		held := e.dp.relay.grants[g2.GetGrantId()]
		return held == nil || len(held.streams) == 0
	})
	// The sweep runs here at several instants before the idle time.
	for _, d := range []time.Duration{0, time.Second, 30 * time.Second} {
		e.dp.relay.sweep(time.Now().Add(d))
		if e.ch.released(g2.GetGrantId()) {
			t.Fatalf("the grant of an unused attach was released %v after an empty stream", d)
		}
	}

	e.openOK(rc, 3, g2.GetGrantId())
	rc.send(datapath.MustNewFrame(3, e.sandbox, attach))
	if f := rc.nextOn(3); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
		t.Fatalf("expected the accepted notice, got %v", f)
	}
	var sawExit bool
	for !sawExit {
		f := rc.nextOn(3)
		sawExit = f.GetExecEvent().GetExited() != nil
	}
	rc.send(datapath.MustNewFrame(3, e.sandbox, &ownsandv1.RelayStreamClose{}))
	waitFor(t, "release after the attach ended", func() bool {
		e.dp.relay.sweep(time.Now())
		return e.ch.released(g2.GetGrantId())
	})
}

// TestRelayUnusedAttachGrantReleasedAtIdle checks that a grant that no
// request used still releases at its idle time.
func TestRelayUnusedAttachGrantReleasedAtIdle(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	g, _ := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, newID(t), nil, 15*time.Minute, 30*time.Minute, "")
	e.dp.relay.sweep(time.Now())
	if e.ch.released(g.GetGrantId()) {
		t.Fatal("the unused grant was released before its idle time")
	}
	e.dp.relay.sweep(time.Now().Add(e.dp.timing.GrantIdle + time.Second))
	if !e.ch.released(g.GrantId) {
		t.Fatal("the unused grant was not released at its idle time")
	}
}

// TestRelayErrorFrameFromControlPlaneEndsTheStreamQuietly checks that an
// error frame of the control plane ends the stream like a stream close frame:
// the agent sends no frame back and does not mark the grant done, so the exec
// continues and its grant is released only when the exec ends.
func TestRelayErrorFrameFromControlPlaneEndsTheStreamQuietly(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "sleep 1; echo done")
	g := e.execGrant(req)
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	if f := rc.nextOn(1); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
		t.Fatalf("expected the accepted notice, got %v", f)
	}
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.ErrorDetail{
		Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED, Message: "unknown frame kind 99 on stream 1"}))
	time.Sleep(200 * time.Millisecond)
	if e.ch.released(g.GetGrantId()) {
		t.Fatal("the grant was released while the exec still ran")
	}
	waitFor(t, "grant release after the exec ended", func() bool { return e.ch.released(g.GetGrantId()) })
	for {
		select {
		case f := <-rc.frames:
			if f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
				continue
			}
			if f.GetError() != nil || f.GetSessionEnd() != nil || f.GetRelayStreamClose() != nil {
				t.Fatalf("the agent replied to the error frame: %v", f)
			}
			if ev := f.GetExecEvent(); ev.GetStdout() != nil || ev.GetExited() != nil {
				t.Fatalf("the agent kept forwarding output to the ended stream: %v", f)
			}
		default:
			return
		}
	}
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

// newManualConn returns a relay and a connection that no loop drives, so a
// test sets the queue and the byte clocks by hand.
func newManualConn(t *testing.T, timing Timing, queue int) (*relay, *rconn) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &relay{d: &DataPlane{timing: timing.withDefaults(), logger: logger()}}
	c := &rconn{out: make(chan *ownsandv1.DataFrame, queue), ctx: ctx, cancel: cancel}
	return r, c
}

// TestRelayWatchdogSkipsKeepaliveOnFullQueue checks that the watchdog never
// waits on the outbound queue: a full queue skips the periodic keepalive
// while recent inbound bytes keep the connection, and a free slot gets the
// keepalive once it is due.
func TestRelayWatchdogSkipsKeepaliveOnFullQueue(t *testing.T) {
	r, c := newManualConn(t, Timing{KeepaliveInterval: time.Second, ReceiveTimeout: time.Minute}, 2)
	mono := time.Hour
	c.clock = func() time.Duration { return mono }
	c.markRecv()
	c.lastSend.Store(0)
	payload := &ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS}
	for i := 0; i < cap(c.out); i++ {
		if err := c.enqueue(payload); err != nil {
			t.Fatal(err)
		}
	}
	done := make(chan bool, 1)
	go func() { done <- r.watchdogTick(c) }()
	select {
	case timedOut := <-done:
		if timedOut {
			t.Fatal("watchdog timed out a connection with recent inbound bytes")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog waited on the full outbound queue")
	}
	if c.ctx.Err() != nil || len(c.out) != cap(c.out) {
		t.Fatalf("skipped keepalive changed the connection: ctx %v, queued %d", c.ctx.Err(), len(c.out))
	}
	// A free slot and a due keepalive: the keepalive is sent, after the
	// queued payload frames.
	<-c.out
	if r.watchdogTick(c) {
		t.Fatal("watchdog timed out a connection with recent inbound bytes")
	}
	<-c.out
	if f := <-c.out; f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE || f.GetRelayKeepalive().GetEchoRequested() {
		t.Fatalf("queued frame %v, want a plain keepalive", f)
	}
	// A keepalive that is not due is not sent.
	c.markSent()
	r.watchdogTick(c)
	if len(c.out) != 0 {
		t.Fatalf("keepalive sent before it was due: %d queued", len(c.out))
	}
	// The receive timeout wins over a full queue.
	for i := 0; i < cap(c.out); i++ {
		c.enqueue(payload)
	}
	c.lastSend.Store(0)
	c.lastRecv.Store(int64(mono - 2*time.Minute))
	if !r.watchdogTick(c) {
		t.Fatal("watchdog did not report the receive timeout with a full queue")
	}
}

// TestRelayWatchdogKeepaliveUsesMonotonicClock checks that the keepalive
// interval follows monotonic time and ignores wall-clock steps, that a
// successful send resets it, and that a full queue skips it without waiting.
func TestRelayWatchdogKeepaliveUsesMonotonicClock(t *testing.T) {
	r, c := newManualConn(t, Timing{KeepaliveInterval: 30 * time.Second, ReceiveTimeout: time.Hour}, 2)
	var mono time.Duration
	c.clock = func() time.Duration { return mono }
	c.markRecv()
	c.markSent()

	// 5 s of monotonic time: not due.
	mono = 5 * time.Second
	if r.watchdogTick(c) || len(c.out) != 0 {
		t.Fatalf("keepalive sent after 5 s of monotonic time: %d queued", len(c.out))
	}
	// 30 s of monotonic time: due exactly once.
	mono = 30 * time.Second
	if r.watchdogTick(c) {
		t.Fatal("watchdog timed out a connection with recent inbound bytes")
	}
	if len(c.out) != 1 {
		t.Fatalf("%d keepalives queued, want 1", len(c.out))
	}
	if f := <-c.out; f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
		t.Fatalf("queued frame %v, want a keepalive", f)
	}
	// A successful send resets the interval.
	c.markSent()
	mono = 59 * time.Second
	r.watchdogTick(c)
	if len(c.out) != 0 {
		t.Fatalf("keepalive sent before the interval after a send: %d queued", len(c.out))
	}
	mono = 60 * time.Second
	r.watchdogTick(c)
	if len(c.out) != 1 {
		t.Fatalf("%d keepalives queued at the interval after a send, want 1", len(c.out))
	}
	// A full queue skips the due keepalive and returns at once.
	c.enqueue(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS})
	mono = 120 * time.Second
	done := make(chan struct{})
	go func() { r.watchdogTick(c); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("watchdog waited on the full outbound queue")
	}
	if len(c.out) != cap(c.out) {
		t.Fatalf("full queue changed: %d queued", len(c.out))
	}
}

// TestRelayReceiveTimeoutUsesMonotonicClock checks that the receive timeout
// follows the elapsed time on the connection's monotonic clock. The injected
// clock is the only time source the watchdog reads, so a wall-clock step in
// either direction cannot move the decision. A received byte, also one of an
// incomplete frame (noteRecv), restarts the interval.
func TestRelayReceiveTimeoutUsesMonotonicClock(t *testing.T) {
	r, c := newManualConn(t, Timing{KeepaliveInterval: time.Hour, ReceiveTimeout: time.Minute}, 2)
	mono := time.Hour
	c.clock = func() time.Duration { return mono }
	c.markRecv()

	mono += time.Minute - time.Nanosecond
	if r.watchdogTick(c) {
		t.Fatal("watchdog timed out the connection before the receive timeout")
	}
	mono += time.Nanosecond
	if !r.watchdogTick(c) {
		t.Fatal("watchdog did not time out the connection at the receive timeout")
	}
	// A partial frame arrives: noteRecv is what the counting body calls.
	c.noteRecv()
	if r.watchdogTick(c) {
		t.Fatal("received bytes did not restart the receive timeout")
	}
	mono += 30 * time.Second
	if r.watchdogTick(c) {
		t.Fatal("watchdog timed out the connection 30 s after the last byte")
	}
	mono += 30 * time.Second
	if !r.watchdogTick(c) {
		t.Fatal("watchdog did not time out the connection 60 s after the last byte")
	}
}

// TestRelayProbeUsesMonotonicClock checks that the echo probe starts only
// after ProbeIdle of monotonic time without a byte, and that a byte counts
// as an answer exactly when it arrived at or after the probe on the same
// clock. An unanswered probe cancels the connection after ProbeTimeout.
func TestRelayProbeUsesMonotonicClock(t *testing.T) {
	r, c := newManualConn(t, Timing{ProbeIdle: 30 * time.Second, ProbeTimeout: 10 * time.Millisecond}, 2)
	mono := time.Hour
	c.clock = func() time.Duration { return mono }
	c.markRecv()

	mono += 30*time.Second - time.Nanosecond
	r.probe(c)
	if len(c.out) != 0 {
		t.Fatalf("probe sent an echo before ProbeIdle: %d queued", len(c.out))
	}

	mono += time.Nanosecond
	r.probe(c)
	select {
	case f := <-c.out:
		if f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE || !f.GetRelayKeepalive().GetEchoRequested() {
			t.Fatalf("queued frame %v, want an echo request", f)
		}
	default:
		t.Fatal("probe sent no echo after ProbeIdle")
	}

	// No byte arrives: the probe timeout cancels the connection.
	select {
	case <-c.ctx.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("an unanswered echo probe did not cancel the connection")
	}
	if !c.probeFailed.Load() {
		t.Fatal("the lost connection was not marked as a probe failure")
	}

	// The answer rule on the monotonic clock.
	sent := mono
	c.lastRecv.Store(int64(sent - time.Nanosecond))
	if c.receivedSince(sent) {
		t.Fatal("a byte before the probe counted as an answer")
	}
	c.lastRecv.Store(int64(sent))
	if !c.receivedSince(sent) {
		t.Fatal("a byte at the probe time did not count as an answer")
	}
	mono += time.Second
	c.noteRecv()
	if !c.receivedSince(sent) {
		t.Fatal("a byte after the probe did not count as an answer")
	}
}

// TestRelayWatchdogRedialsWhenWriterIsBlocked checks the whole loop: with a
// live grant, a full outbound queue, a writer that cannot send (the control
// plane does not read the stream, so HTTP/2 flow control stops it) and no
// inbound bytes, the agent cancels the connection at the receive timeout
// and dials again.
//
// The test first proves that precondition on the first connection: its
// outbound queue is full and its writer is blocked inside Send (the time of
// the last successful send stops advancing while the queue stays full). A
// silent connection with a spare queue would also redial at the receive
// timeout, so a missing precondition fails the test with its own message
// instead of passing on the redial.
func TestRelayWatchdogRedialsWhenWriterIsBlocked(t *testing.T) {
	const (
		keepalive      = 100 * time.Millisecond
		receiveTimeout = 2 * time.Second
		// setupBound limits each wait for a precondition. It only guards
		// against a hang, because the control plane feeds the connection
		// with keepalives during the setup, so the receive timeout cannot
		// expire before the precondition holds.
		setupBound = 20 * time.Second
	)
	e := newRelayEnv(t, Timing{KeepaliveInterval: keepalive, ReceiveTimeout: receiveTimeout, ProbeIdle: time.Hour}, RelayLimits{})
	var stalled atomic.Bool
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	var count atomic.Int32
	e.cp.SetRelayHandler(func(ctx context.Context, _ string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		rc := &relayConn{t: t, ctx: ctx, frames: make(chan *ownsandv1.DataFrame, 4096), stream: stream}
		e.conns <- rc
		if count.Add(1) > 1 {
			return nil
		}
		// First connection: read the startup frames, then stop reading and
		// stay silent.
		for i := 0; i < 2; i++ {
			if _, err := stream.Receive(); err != nil {
				return nil
			}
		}
		stalled.Store(true)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	})
	// The output (1 GB) and the credit window (1 GiB) are far larger than
	// what 256 queued frames hold, whatever the size of the output chunks,
	// so the outbound queue fills on a loaded machine too.
	req := e.execReq("/bin/sh", "-c", "head -c 1000000000 /dev/zero")
	g := e.execGrant(req)
	// The connection loss ends the stream of g and releases it; this second
	// grant stays live, so the agent has a reason to dial again.
	e.execGrant(e.execReq("/bin/true"))
	// Receive the connection directly: e.conn() would also wait for a frame
	// that this custom handler never queues. A bounded wait turns a
	// connection that never arrives into a diagnostic instead of the outer
	// test timeout.
	var rc *relayConn
	select {
	case rc = <-e.conns:
	case <-time.After(10 * time.Second):
		t.Fatal("no initial relay connection")
	}
	waitFor(t, "stalled relay handler", stalled.Load)
	e.dp.relay.mu.Lock()
	first := e.dp.relay.conn
	e.dp.relay.mu.Unlock()
	if first == nil {
		t.Fatal("setup: no relay connection is installed after the first handler stalled")
	}
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamOpen{GrantId: g.GetGrantId(), InitialWindowBytes: 1 << 30}))
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))

	// The first handler is silent, and the receive timeout counts from the
	// last inbound byte, so keepalives of the control plane keep the
	// connection alive until both preconditions are proven. The feed stops
	// before the watchdog check, from which on the connection is silent.
	stopFeed := make(chan struct{})
	feedDone := make(chan struct{})
	var stopOnce sync.Once
	stopKeepalives := func() {
		stopOnce.Do(func() { close(stopFeed) })
		<-feedDone
	}
	t.Cleanup(stopKeepalives)
	go func() {
		defer close(feedDone)
		ticker := time.NewTicker(keepalive)
		defer ticker.Stop()
		for {
			select {
			case <-stopFeed:
				return
			case <-ticker.C:
				if err := rc.stream.Send(&ownsandv1.ConnectResponse{Frame: keepaliveFrame(false)}); err != nil {
					return
				}
			}
		}
	}()

	// Precondition 1: the sandbox output fills the outbound queue, because
	// the writer no longer drains it.
	setupDeadline := time.Now().Add(setupBound)
	pollSetup := func(what string, cond func() bool) {
		t.Helper()
		for !cond() {
			select {
			case <-e.conns:
				t.Fatalf("setup: the agent dialed again while waiting: %s", what)
			default:
			}
			if time.Now().After(setupDeadline) {
				t.Fatalf("setup: %s within %s (queue %d of %d, last send %s ago)",
					what, setupBound, len(first.out), cap(first.out), first.sinceSend())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	pollSetup("the first connection outbound queue never filled", func() bool { return len(first.out) == cap(first.out) })

	// Precondition 2: the writer is blocked mid-send. The queue is full and
	// no send succeeds for two keepalive intervals. A writer that only
	// waits on an empty queue would not leave the queue full.
	lastSend := first.lastSend.Load()
	pollSetup("the first connection writer never proved blocked in Send", func() bool {
		if cur := first.lastSend.Load(); cur != lastSend {
			// A send completed, so the writer was still draining: restart
			// the observation from the new send time.
			lastSend = cur
			return false
		}
		return len(first.out) == cap(first.out) && first.sinceSend() >= 2*keepalive
	})

	// The watchdog cancels the blocked connection at the receive timeout,
	// and the spare grant makes the agent dial again.
	stopKeepalives()
	select {
	case <-first.ctx.Done():
	case <-time.After(3 * receiveTimeout):
		t.Fatal("the watchdog did not cancel the blocked connection after the receive timeout")
	}
	select {
	case <-e.conns:
	case <-time.After(15 * time.Second):
		t.Fatal("the agent did not dial again after the receive timeout")
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

// A credential request that never returns is canceled when its agent
// channel closes, so a grant of the next channel gets a fresh credential
// request and a connection without waiting for the old response.
func TestRelayCancelsPendingCredentialOnChannelLoss(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: 300 * time.Millisecond}, RelayLimits{})
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	canceled := make(chan struct{})
	hook := func(ctx context.Context) error {
		if calls.Add(1) > 1 {
			return nil
		}
		entered <- struct{}{}
		// The first request returns only when it is canceled.
		<-ctx.Done()
		close(canceled)
		return ctx.Err()
	}
	e.beforeAuth.Store(&hook)
	e.execGrant(e.execReq("/bin/true"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relay loop did not request a credential")
	}
	// The channel closes first, as in the agent, then the data plane learns
	// about the loss.
	e.ch.Close()
	e.dp.ChannelLost(e.ch)
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("the pending credential request was not canceled with its channel")
	}
	e.ch = newChannel(2)
	e.dp.ChannelOpened(e.ch)
	g := e.execGrant(e.execReq("/bin/true"))
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
	if n := calls.Load(); n != 2 {
		t.Fatalf("credential requests = %d, want 2", n)
	}
}

// A credential that arrives after its channel closed cannot start a
// connection, and a new channel without a live grant makes no dial.
func TestRelayDropsCredentialOfClosedChannel(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: 300 * time.Millisecond}, RelayLimits{})
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	hook := func(context.Context) error {
		// The old response completes during the teardown and ignores the
		// cancellation of its request.
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-gate
		}
		return nil
	}
	e.beforeAuth.Store(&hook)
	e.execGrant(e.execReq("/bin/true"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relay loop did not request a credential")
	}
	e.ch.Close()
	e.dp.ChannelLost(e.ch)
	close(gate)
	e.noConn(500 * time.Millisecond)
	// A new channel without a grant gets no relay dial either.
	e.ch = newChannel(2)
	e.dp.ChannelOpened(e.ch)
	e.noConn(500 * time.Millisecond)
	if n := calls.Load(); n != 1 {
		t.Fatalf("credential requests = %d, want 1 without a live grant", n)
	}
	// A grant of the new channel starts a connection again.
	g := e.execGrant(e.execReq("/bin/true"))
	rc := e.conn()
	e.openOK(rc, 1, g.GetGrantId())
}

// noConn fails when a relay connection opens within d.
func (e *relayEnv) noConn(d time.Duration) {
	e.t.Helper()
	select {
	case <-e.conns:
		e.t.Fatal("a relay connection opened without a live grant of the current channel")
	case <-time.After(d):
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

// logBuffer is a log sink that a test reads while the relay writes to it.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// revokeFixture is a relay with one hand-built connection whose outbound
// queue nobody drains, which models a writer that the transport blocks.
type revokeFixture struct {
	t    *testing.T
	r    *relay
	c    *rconn
	logs *logBuffer
}

func newRevokeFixture(t *testing.T, queue int) *revokeFixture {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	logs := &logBuffer{}
	d := &DataPlane{timing: Timing{}.withDefaults(), logger: slog.New(slog.NewJSONHandler(logs, nil))}
	r := &relay{d: d, limits: DefaultRelayLimits(), grants: map[string]*grant{}}
	c := &rconn{gen: 7, out: make(chan *ownsandv1.DataFrame, queue), ctx: ctx, cancel: cancel, streams: map[uint64]*rstream{}}
	r.conn = c
	return &revokeFixture{t: t, r: r, c: c, logs: logs}
}

// grant installs a grant with one stream per given ID on the connection.
func (f *revokeFixture) grant(deadlineIn time.Duration, received time.Time, ids ...uint64) (*grant, []*rstream) {
	g := &grant{id: newID(f.t), sandboxID: "sandbox", kind: ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC,
		ch: newChannel(7), received: received, expiresIn: time.Hour, deadlineIn: deadlineIn, streams: map[*rstream]bool{}, opened: true}
	var streams []*rstream
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	f.r.grants[g.id] = g
	for _, id := range ids {
		ctx, cancel := context.WithCancel(f.c.ctx)
		st := &rstream{id: id, g: g, conn: f.c, ctx: ctx, cancel: cancel}
		g.streams[st] = true
		f.c.streams[id] = st
		streams = append(streams, st)
	}
	return g, streams
}

func (f *revokeFixture) fillQueue() {
	f.t.Helper()
	for i := 0; i < cap(f.c.out); i++ {
		if !f.c.tryEnqueue(keepaliveFrame(false)) {
			f.t.Fatal("queue full too early")
		}
	}
}

// within runs fn and fails the test when it does not return in time.
func within(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s waited on the full outbound queue", what)
	}
}

func (f *revokeFixture) streamCount() int {
	f.r.mu.Lock()
	defer f.r.mu.Unlock()
	return len(f.c.streams)
}

// TestRelayRevokeWithFullQueueClosesConnection checks that a revocation
// never waits on a full outbound queue behind a blocked writer: it cancels
// the affected streams and the connection, leaves the queue untouched and
// logs the fallback once for the connection. The agent channel receive
// loop calls revoke before it acknowledges the revocation, so the return
// of revoke is what keeps the channel available.
func TestRelayRevokeWithFullQueueClosesConnection(t *testing.T) {
	f := newRevokeFixture(t, 2)
	g, streams := f.grant(time.Hour, time.Now(), 1, 2)
	_, other := f.grant(time.Hour, time.Now(), 3)
	f.fillQueue()

	within(t, "revoke", func() { f.r.revoke(g.id) })

	if f.c.ctx.Err() == nil {
		t.Fatal("the connection that could not take the terminal frame was not canceled")
	}
	for _, st := range streams {
		if st.ctx.Err() == nil || !st.ended {
			t.Fatalf("stream %d of the revoked grant was not ended", st.id)
		}
	}
	if len(f.c.out) != cap(f.c.out) {
		t.Fatalf("the full queue changed: %d queued", len(f.c.out))
	}
	f.r.mu.Lock()
	_, kept := f.r.grants[g.id]
	f.r.mu.Unlock()
	if kept {
		t.Fatal("the revoked grant stays installed")
	}
	// The unrelated grant stays installed; the closed connection ends its
	// stream like any connection loss.
	f.r.mu.Lock()
	_, otherKept := f.r.grants[other[0].g.id]
	f.r.mu.Unlock()
	if !otherKept {
		t.Fatal("an unrelated grant was removed")
	}
	f.r.connClosed(f.c)
	if f.streamCount() != 0 || other[0].ctx.Err() == nil {
		t.Fatal("the connection close did not end the remaining stream")
	}
	logs := f.logs.String()
	if n := strings.Count(logs, "outbound queue is full"); n != 1 {
		t.Fatalf("queue-full fallback logged %d times, want once per connection:\n%s", n, logs)
	}
	for _, want := range []string{`"generation":7`, `"termination_reason":"SESSION_END_REASON_REVOKED"`} {
		if !strings.Contains(logs, want) {
			t.Fatalf("log lacks %s:\n%s", want, logs)
		}
	}
}

// TestRelayRevokeWithQueueSpaceSendsRevoked checks that the fallback does
// not change the normal path: the revoked stream gets its REVOKED frame
// and the connection and an unrelated grant stay in use.
func TestRelayRevokeWithQueueSpaceSendsRevoked(t *testing.T) {
	f := newRevokeFixture(t, 4)
	g, streams := f.grant(time.Hour, time.Now(), 1)
	_, other := f.grant(time.Hour, time.Now(), 3)

	within(t, "revoke", func() { f.r.revoke(g.id) })

	select {
	case fr := <-f.c.out:
		if fr.GetStreamId() != 1 || fr.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
			t.Fatalf("queued frame %v, want REVOKED on stream 1", fr)
		}
	default:
		t.Fatal("no session-end frame was queued")
	}
	if f.c.ctx.Err() != nil || other[0].ctx.Err() != nil || other[0].ended {
		t.Fatal("a revocation with queue space affected the connection or an unrelated stream")
	}
	if streams[0].ctx.Err() == nil || !streams[0].ended {
		t.Fatal("the revoked stream was not ended")
	}
	if logs := f.logs.String(); strings.Contains(logs, "outbound queue is full") {
		t.Fatalf("unexpected fallback log:\n%s", logs)
	}
}

// TestRelayDeadlineSweepWithQueueSpaceAndFullQueue checks the stream
// deadline sweep, which runs on the housekeeping goroutine: with a free
// slot the stream ends with EXPIRED, with a full queue the sweep returns
// and closes the connection.
func TestRelayDeadlineSweepWithQueueSpaceAndFullQueue(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	t.Run("queue space", func(t *testing.T) {
		f := newRevokeFixture(t, 4)
		_, streams := f.grant(time.Minute, past, 1)
		within(t, "sweep", func() { f.r.sweep(time.Now()) })
		select {
		case fr := <-f.c.out:
			if fr.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED {
				t.Fatalf("queued frame %v, want EXPIRED", fr)
			}
		default:
			t.Fatal("no session-end frame was queued")
		}
		if f.c.ctx.Err() != nil || !streams[0].ended {
			t.Fatal("the deadline ended the connection or kept the stream")
		}
	})
	t.Run("full queue", func(t *testing.T) {
		f := newRevokeFixture(t, 2)
		_, streams := f.grant(time.Minute, past, 1)
		f.fillQueue()
		within(t, "sweep", func() { f.r.sweep(time.Now()) })
		if f.c.ctx.Err() == nil || !streams[0].ended {
			t.Fatal("the full queue did not close the connection and end the stream")
		}
		if n := strings.Count(f.logs.String(), `"termination_reason":"SESSION_END_REASON_EXPIRED"`); n != 1 {
			t.Fatalf("fallback logged %d times\n%s", n, f.logs.String())
		}
	})
}

// TestRelayRevokeKeepsUnrelatedGrantRunning runs the whole path on a real
// relay connection: the named grant's stream gets REVOKED and an exec of an
// unrelated grant on the same connection continues to its result.
func TestRelayRevokeKeepsUnrelatedGrantRunning(t *testing.T) {
	e := newRelayEnv(t, Timing{}, RelayLimits{})
	long := e.execReq("/bin/sh", "-c", "sleep 30")
	g1 := e.execGrant(long)
	rc := e.conn()
	e.openOK(rc, 1, g1.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, long))
	rc.nextOn(1) // accepted

	other := e.execReq("/bin/sh", "-c", "sleep 1; echo alive")
	g2 := e.execGrant(other)
	e.openOK(rc, 2, g2.GetGrantId())
	rc.send(datapath.MustNewFrame(2, e.sandbox, other))
	rc.nextOn(2) // accepted

	e.dp.Revoke(e.ch, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_RelayGrantId{RelayGrantId: g1.GetGrantId()}})
	for {
		f := rc.nextOn(1)
		if f.GetSessionEnd() != nil {
			if f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
				t.Fatalf("revocation %v", f)
			}
			break
		}
	}
	var out string
	for {
		ev := rc.nextOn(2).GetExecEvent()
		out += string(ev.GetStdout().GetData())
		if ev.GetExited() != nil {
			break
		}
	}
	if out != "alive\n" {
		t.Fatalf("unrelated exec output %q", out)
	}
	if rc.ctx.Err() != nil {
		t.Fatal("the revocation closed the relay connection")
	}
}

func (e *relayEnv) relayRunning() bool {
	e.dp.relay.mu.Lock()
	defer e.dp.relay.mu.Unlock()
	return e.dp.relay.running
}

// gatedGrant installs one exec grant and returns once the loop waits in
// the credential request. The returned function releases the request.
func (e *relayEnv) gatedGrant(t *testing.T) (*ownsandv1.RelayGrant, func()) {
	t.Helper()
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	hook := func(context.Context) error {
		if calls.Add(1) == 1 {
			entered <- struct{}{}
			<-gate
		}
		return nil
	}
	e.beforeAuth.Store(&hook)
	g := e.execGrant(e.execReq("/bin/true"))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("relay loop did not request a credential")
	}
	return g, func() { close(gate) }
}

func (e *relayEnv) revokeGrant(g *ownsandv1.RelayGrant) {
	e.dp.Revoke(e.ch, &ownsandv1.SessionRevocation{RevocationId: newID(e.t), Target: &ownsandv1.SessionRevocation_RelayGrantId{RelayGrantId: g.GetGrantId()}})
}

// A grant that is revoked while the credential is requested leaves no live
// grant, so the loop makes no dial and stops; a later grant starts again.
func TestRelayNoDialWhenOnlyGrantRevokedDuringCredentialRequest(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: 300 * time.Millisecond}, RelayLimits{})
	g, release := e.gatedGrant(t)
	e.revokeGrant(g)
	release()
	e.noConn(500 * time.Millisecond)
	deadline := time.Now().Add(5 * time.Second)
	for e.relayRunning() {
		if time.Now().After(deadline) {
			t.Fatal("relay loop did not stop without a live grant")
		}
		time.Sleep(10 * time.Millisecond)
	}
	g2 := e.execGrant(e.execReq("/bin/true"))
	rc := e.conn()
	e.openOK(rc, 1, g2.GetGrantId())
}

// Another live grant of the same channel keeps the one relay connection.
func TestRelayDialsWhenAnotherGrantLiveAfterCredentialRequest(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: 300 * time.Millisecond}, RelayLimits{})
	g, release := e.gatedGrant(t)
	g2 := e.execGrant(e.execReq("/bin/true"))
	e.revokeGrant(g)
	release()
	rc := e.conn()
	e.openOK(rc, 1, g2.GetGrantId())
	e.noConn(300 * time.Millisecond)
}

// TestRelayProbeWithFullQueueSkipsEchoAndKeepsTimeout checks that the echo
// probe, which grant runs on the agent channel receive loop, never waits on
// a full outbound queue: it queues nothing, logs the skip, and the timeout
// still ends a connection that receives no byte, while a received byte keeps
// the connection.
func TestRelayProbeWithFullQueueSkipsEchoAndKeepsTimeout(t *testing.T) {
	timing := Timing{ProbeIdle: 50 * time.Millisecond, ProbeTimeout: 200 * time.Millisecond}
	for _, answered := range []bool{false, true} {
		f := newRevokeFixture(t, 2)
		f.r.d.timing = timing.withDefaults()
		f.r.d.logger = slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
		f.c.lastRecv.Store(int64(f.c.monoNow() - time.Hour))
		f.fillQueue()

		within(t, "probe", func() { f.r.probe(f.c) })

		if len(f.c.out) != cap(f.c.out) {
			t.Fatalf("the full queue changed: %d queued", len(f.c.out))
		}
		if n := strings.Count(f.logs.String(), "echo probe skipped"); n != 1 {
			t.Fatalf("skipped echo logged %d times, want once:\n%s", n, f.logs.String())
		}
		if answered {
			f.c.markRecv()
		}
		time.Sleep(500 * time.Millisecond)
		if answered {
			if f.c.ctx.Err() != nil || f.c.probeFailed.Load() {
				t.Fatal("a byte within the probe timeout did not keep the connection")
			}
		} else if f.c.ctx.Err() == nil || !f.c.probeFailed.Load() {
			t.Fatal("the probe timeout did not cancel the idle connection")
		}
	}
}

// blockedSendFixture is a relay with a one-slot outbound queue that nobody
// drains and one admitted stream with credit, so an operation send or the
// request accepted notice blocks on the queue.
func blockedSendFixture(t *testing.T) (*revokeFixture, *grant, *rstream) {
	t.Helper()
	f := newRevokeFixture(t, 1)
	g, streams := f.grant(time.Hour, time.Now(), 1)
	st := streams[0]
	st.send, _ = datapath.NewSendWindow(1 << 20)
	st.recv, _ = datapath.NewReceiveWindow(f.r.limits.StreamReceiveWindow)
	f.c.credit = f.r.limits.StreamReceiveWindow
	f.fillQueue()
	return f, g, st
}

func closeFrame(id uint64) *ownsandv1.DataFrame {
	return datapath.MustNewFrame(id, "sandbox", &ownsandv1.RelayStreamClose{})
}

// TestRelayStreamCloseCancelsBlockedOperationSend checks that a stream close
// frame ends an operation send that waits on a full outbound queue, before the
// queue drains, and that no frame of the closed stream is queued afterwards,
// while the connection keeps serving other streams.
func TestRelayStreamCloseCancelsBlockedOperationSend(t *testing.T) {
	f, g, st := blockedSendFixture(t)
	frame := datapath.MustNewFrame(1, "sandbox", &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte("payload")}}})
	result := make(chan error, 1)
	go func() { result <- relaySink{st}.Send(st.ctx, frame) }()
	select {
	case err := <-result:
		t.Fatalf("send returned %v while the queue was full", err)
	case <-time.After(100 * time.Millisecond):
	}

	f.r.onFrame(f.c, closeFrame(1))
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("send returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the blocked send was not canceled by the stream close frame")
	}
	if len(f.c.out) != cap(f.c.out) {
		t.Fatalf("queue changed before it drained: %d frames", len(f.c.out))
	}

	// Only the frame queued before the blocked send remains.
	if first := <-f.c.out; first.GetKind() != keepaliveFrame(false).GetKind() {
		t.Fatalf("queued frame %v", first)
	}
	time.Sleep(100 * time.Millisecond)
	if len(f.c.out) != 0 {
		t.Fatalf("a frame of the closed stream was queued after the drain: %d frames", len(f.c.out))
	}
	// A send on the canceled stream context queues nothing, even with room.
	if err := (relaySink{st}).Send(st.ctx, frame); !errors.Is(err, context.Canceled) || len(f.c.out) != 0 {
		t.Fatalf("send after the close returned %v with %d queued", err, len(f.c.out))
	}

	// The connection stays open and serves another stream.
	if f.c.ctx.Err() != nil {
		t.Fatal("the connection was closed")
	}
	ctx, cancel := context.WithCancel(f.c.ctx)
	defer cancel()
	other := &rstream{id: 3, g: g, conn: f.c, ctx: ctx, cancel: cancel}
	other.send, _ = datapath.NewSendWindow(1 << 20)
	if err := (relaySink{other}).Send(ctx, datapath.MustNewFrame(3, "sandbox", &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte("other")}}})); err != nil {
		t.Fatalf("send on another stream: %v", err)
	}
	if got := <-f.c.out; got.GetStreamId() != 3 {
		t.Fatalf("queued frame %v", got)
	}
}

// TestRelayStreamCloseCancelsBlockedRequestAccepted checks that the request
// accepted notice waits on the stream context: a close frame ends the wait and
// the notice is never queued afterwards.
//
// The grant starts unopened and the one-slot queue stays full with the
// original keepalive. The test proves with a probe context (see
// waitProbeContext) that the notice reached its wait on the stream context
// with a live stream and a full queue before it delivers the close. A close
// that arrived earlier would take the early return of enqueueCtx and pass
// without the wait. After the close, the dispatcher must return while the
// queue is still full, which a wait on the connection context would not allow
// because nothing cancels the connection. Every wait has a bound that only
// guards against a hang.
func TestRelayStreamCloseCancelsBlockedRequestAccepted(t *testing.T) {
	const bound = 10 * time.Second
	f, g, st := blockedSendFixture(t)
	f.r.mu.Lock()
	g.opened = false
	f.r.mu.Unlock()
	probe := newWaitProbeContext(st.ctx)
	st.ctx = probe
	d := &acceptNoticeDispatcher{probe: probe, accepted: make(chan struct{})}
	f.r.d.opts.Dispatcher = d
	req := &ownsandv1.ExecRequest{OrganizationId: orgID, SandboxId: g.sandboxID, ExecId: newID(t), Command: []string{"/bin/true"}, BindingSalt: make([]byte, 32)}

	f.r.onFrame(f.c, datapath.MustNewFrame(st.id, g.sandboxID, req))
	// The dispatcher accepted the request and now waits to queue the notice.
	select {
	case <-probe.waited:
	case <-time.After(bound):
		t.Fatal("the request accepted notice never reached its wait on the stream context")
	}
	f.r.mu.Lock()
	opened := g.opened
	f.r.mu.Unlock()
	if !opened {
		t.Fatal("the accepted request did not open the grant")
	}
	if probe.Context.Err() != nil {
		t.Fatal("the stream was already closed when the notice began to wait")
	}
	if len(f.c.out) != cap(f.c.out) {
		t.Fatalf("the notice was queued on a full queue: %d frames", len(f.c.out))
	}
	if st.recv.Buffered() == 0 {
		t.Fatal("the request frame is not buffered while the dispatcher runs")
	}
	select {
	case <-d.accepted:
		t.Fatal("the notice returned before the stream closed")
	default:
	}

	f.r.onFrame(f.c, closeFrame(st.id))
	select {
	case <-d.accepted:
	case <-time.After(bound):
		t.Fatal("the stream close did not end the wait of the request accepted notice")
	}
	// The dispatcher returns and the relay frees the request frame, still
	// with a full queue: the notice was dropped, not queued.
	waitFor(t, "the dispatcher to return", func() bool { return st.recv.Buffered() == 0 })
	if len(f.c.out) != cap(f.c.out) {
		t.Fatalf("the queue changed after the close: %d frames", len(f.c.out))
	}
	// Only the keepalive that filled the queue remains.
	if first := <-f.c.out; first.GetKind() != keepaliveFrame(false).GetKind() {
		t.Fatalf("queued frame %v", first)
	}
	if len(f.c.out) != 0 {
		t.Fatalf("a frame of the closed stream was queued after the close: %d frames", len(f.c.out))
	}
	if f.c.ctx.Err() != nil {
		t.Fatal("the connection was closed")
	}
}

// grantReceived returns the instant at which the relay installed grant g, so
// a test can drive sweep at exact offsets from the grant's expiry.
func (e *relayEnv) grantReceived(g *ownsandv1.RelayGrant) time.Time {
	e.t.Helper()
	e.dp.relay.mu.Lock()
	defer e.dp.relay.mu.Unlock()
	held := e.dp.relay.grants[g.GetGrantId()]
	if held == nil {
		e.t.Fatal("the grant is not installed")
	}
	return held.received
}

// waitNoStreams waits until grant g holds no open stream.
func (e *relayEnv) waitNoStreams(g *ownsandv1.RelayGrant) {
	e.t.Helper()
	waitFor(e.t, "the streams of the grant to end", func() bool {
		e.dp.relay.mu.Lock()
		defer e.dp.relay.mu.Unlock()
		held := e.dp.relay.grants[g.GetGrantId()]
		return held == nil || len(held.streams) == 0
	})
}

// TestRelayIdleGrantWaitsForAdmissionToleranceAfterExpiry checks that a
// grant whose last stream ended before its expiry (or that never admitted a
// stream) stays admittable for the 30-second admission tolerance after its
// expiry, as the contract says, and is released only after that. The agent
// and the control plane then agree on which grants can still admit a first
// request frame.
func TestRelayIdleGrantWaitsForAdmissionToleranceAfterExpiry(t *testing.T) {
	const expiry = 15 * time.Minute
	for _, tc := range []string{"never_opened", "stream_closed_before_expiry", "request_accepted_then_closed"} {
		t.Run(tc, func(t *testing.T) {
			e := newRelayEnv(t, Timing{GrantIdle: time.Hour, AdmissionTolerance: 30 * time.Second}, RelayLimits{})
			req := e.execReq("/bin/sh", "-c", "sleep 30")
			g, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.GetExecId(), relaybinding.ExecDigest(req), expiry, time.Hour, "")
			if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
				t.Fatalf("grant ack %v", ack)
			}
			if tc != "never_opened" {
				rc := e.conn()
				e.openOK(rc, 1, g.GetGrantId())
				if tc == "request_accepted_then_closed" {
					rc.send(datapath.MustNewFrame(1, e.sandbox, req))
					if f := rc.nextOn(1); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
						t.Fatalf("expected the accepted notice, got %v", f)
					}
				}
				rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamClose{}))
				e.waitNoStreams(g)
			}
			received := e.grantReceived(g)
			// Before the expiry and shortly after it, the grant is held and
			// admittable.
			for _, d := range []time.Duration{expiry - time.Second, expiry + time.Second, expiry + 5*time.Second, expiry + 29*time.Second} {
				e.dp.relay.sweep(received.Add(d))
				if e.ch.released(g.GetGrantId()) {
					t.Fatalf("the grant was released %v after its installation, before expiry plus the admission tolerance", d)
				}
			}
			e.dp.relay.mu.Lock()
			held := e.dp.relay.grants[g.GetGrantId()]
			admittable := held != nil && held.admittable(received.Add(expiry+5*time.Second), e.dp.timing.AdmissionTolerance)
			e.dp.relay.mu.Unlock()
			if !admittable {
				t.Fatal("the grant is not admittable 5 seconds after its expiry")
			}
			e.dp.relay.sweep(received.Add(expiry + 31*time.Second))
			if !e.ch.released(g.GetGrantId()) {
				t.Fatal("the grant was not released after its expiry plus the admission tolerance")
			}
		})
	}
}

// TestRelayGrantWhoseLastStreamEndedAfterExpiryReleasesAtOnce checks that a
// grant whose last stream ended at or after the grant's expiry is released
// without waiting for the admission tolerance, and that an open stream keeps
// the grant.
func TestRelayGrantWhoseLastStreamEndedAfterExpiryReleasesAtOnce(t *testing.T) {
	e := newRelayEnv(t, Timing{GrantIdle: time.Hour, AdmissionTolerance: 30 * time.Second}, RelayLimits{})
	g, _ := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, newID(t), nil, 15*time.Minute, time.Hour, "")
	received := e.grantReceived(g)
	late := received.Add(15*time.Minute + 2*time.Second)
	e.dp.relay.mu.Lock()
	held := e.dp.relay.grants[g.GetGrantId()]
	held.streams[&rstream{}] = true
	held.idleSince = late.Add(-time.Hour)
	e.dp.relay.mu.Unlock()
	e.dp.relay.sweep(late)
	if e.ch.released(g.GetGrantId()) {
		t.Fatal("a grant with an open stream was released")
	}
	// The last stream ends after the expiry, as finishStream records it.
	e.dp.relay.mu.Lock()
	clear(held.streams)
	held.idleSince = late
	e.dp.relay.mu.Unlock()
	e.dp.relay.sweep(late)
	if !e.ch.released(g.GetGrantId()) {
		t.Fatal("the grant whose last stream ended after its expiry was not released at once")
	}
}

// TestRelayExpiredGrantAdmitsStreamWithinToleranceInRealTime runs the
// expiry with real time: with an expiry of 1 second and an admission
// tolerance of 8 seconds, an idle grant still admits a stream and its
// request 2.2 seconds after its installation, without a release message, and
// is released as soon as that stream ends after the expiry.
func TestRelayExpiredGrantAdmitsStreamWithinToleranceInRealTime(t *testing.T) {
	e := newRelayEnv(t, Timing{GrantIdle: time.Hour, AdmissionTolerance: 8 * time.Second}, RelayLimits{})
	req := e.execReq("/bin/sh", "-c", "sleep 30")
	start := time.Now()
	g, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.GetExecId(), relaybinding.ExecDigest(req), time.Second, time.Hour, "")
	if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
		t.Fatalf("grant ack %v", ack)
	}
	rc := e.conn()
	time.Sleep(time.Until(start.Add(2200 * time.Millisecond)))
	if e.ch.released(g.GetGrantId()) {
		t.Fatal("the idle grant was released at its expiry, before the admission tolerance ended")
	}
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	if f := rc.nextOn(1); f.GetKind() != ownsandv1.DataFrameKind_DATA_FRAME_KIND_REQUEST_ACCEPTED {
		t.Fatalf("a request within the admission tolerance got %v", f)
	}
	if e.ch.released(g.GetGrantId()) {
		t.Fatal("the grant was released while its stream was open")
	}
	ended := time.Now()
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamClose{}))
	waitFor(t, "release after the stream ended past the expiry", func() bool { return e.ch.released(g.GetGrantId()) })
	if d := time.Since(ended); d > 3*time.Second {
		t.Fatalf("the grant was released %v after its last stream ended past the expiry", d)
	}
}

// TestRelayCloseAfterFlushIsBoundedWhenWriterNeverReachesMarker checks that
// the shutdown deadline of a lost channel's connection covers the whole
// flush, not only the enqueue of the flush marker: when the queue takes the
// marker but the writer never reaches it (a writer that HTTP/2 flow control
// blocks), and when the writer took the marker but its request close never
// returns, the connection is still canceled at the deadline.
func TestRelayCloseAfterFlushIsBoundedWhenWriterNeverReachesMarker(t *testing.T) {
	const deadline = 300 * time.Millisecond
	for _, tc := range []string{"marker_never_dequeued", "marker_taken_close_request_stalls", "queue_full"} {
		t.Run(tc, func(t *testing.T) {
			_, c := newManualConn(t, Timing{}, 4)
			c.flushDeadline = deadline
			switch tc {
			case "marker_taken_close_request_stalls":
				// The writer dequeues the marker and then blocks in the
				// request close; it never starts its own 2-second timer.
				go func() { <-c.out; select {} }()
			case "queue_full":
				for i := 0; i < cap(c.out); i++ {
					c.out <- keepaliveFrame(false)
				}
			}
			start := time.Now()
			go c.closeAfterFlush()
			select {
			case <-c.ctx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the connection was not canceled after the shutdown deadline")
			}
			if d := time.Since(start); d < deadline*8/10 || d > deadline+time.Second {
				t.Fatalf("the connection was canceled after %v, want about %v", d, deadline)
			}
		})
	}
}

// TestRelayCloseAfterFlushStopsItsTimerWhenTheConnectionEnds checks that a
// connection that ends before the deadline is not canceled again: the timer
// stops with the connection's context.
func TestRelayCloseAfterFlushStopsItsTimerWhenTheConnectionEnds(t *testing.T) {
	_, c := newManualConn(t, Timing{}, 4)
	c.flushDeadline = 200 * time.Millisecond
	canceled := make(chan struct{}, 2)
	inner := c.cancel
	c.cancel = func() { canceled <- struct{}{}; inner() }
	go func() { <-c.out; inner() }()
	c.closeAfterFlush()
	time.Sleep(600 * time.Millisecond)
	if len(canceled) != 0 {
		t.Fatal("the shutdown timer canceled a connection that had ended")
	}
}

// TestRelayChannelLossCancelsConnectionWithBlockedWriter runs the lost
// channel with the real transport: the control plane stops reading the
// relay stream, so HTTP/2 flow control blocks the agent's writer while the
// outbound queue still has room and the control plane keeps sending bytes.
// The queue takes the flush marker, but the writer never reaches it. The
// agent must still cancel the connection at the shutdown deadline, and the
// live grant of the new channel must get a new connection after the bounded
// teardown.
func TestRelayChannelLossCancelsConnectionWithBlockedWriter(t *testing.T) {
	const flush = time.Second
	e := newRelayEnv(t, Timing{FlushDeadline: flush, ReceiveTimeout: time.Hour, KeepaliveInterval: time.Hour, ProbeIdle: time.Hour}, RelayLimits{})
	var (
		count   atomic.Int32
		stalled atomic.Bool
		sendMu  sync.Mutex
		release = make(chan struct{})
	)
	t.Cleanup(func() { close(release) })
	e.cp.SetRelayHandler(func(ctx context.Context, _ string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		rc := &relayConn{t: t, ctx: ctx, frames: make(chan *ownsandv1.DataFrame, 4096), stream: stream}
		e.conns <- rc
		if count.Add(1) > 1 {
			for {
				req, err := stream.Receive()
				if err != nil {
					return nil
				}
				rc.frames <- req.GetFrame()
			}
		}
		// First connection: read the startup frames, then stop reading, so
		// the transport blocks the writer, but keep sending keepalives.
		for i := 0; i < 2; i++ {
			if _, err := stream.Receive(); err != nil {
				return nil
			}
		}
		stalled.Store(true)
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return nil
			case <-tick.C:
				sendMu.Lock()
				_ = stream.Send(&ownsandv1.ConnectResponse{Frame: keepaliveFrame(false)})
				sendMu.Unlock()
			}
		}
	})
	req := e.execReq("/bin/sh", "-c", "head -c 3000000 /dev/zero; sleep 30")
	g := e.execGrant(req)
	rc := <-e.conns
	waitFor(t, "stalled relay handler", stalled.Load)
	sendMu.Lock()
	rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamOpen{GrantId: g.GetGrantId(), InitialWindowBytes: 64 << 20}))
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	sendMu.Unlock()
	e.dp.relay.mu.Lock()
	old := e.dp.relay.conn
	e.dp.relay.mu.Unlock()
	// Wait until the writer is blocked: the queue stops draining while it
	// still has free slots.
	var queued int
	waitFor(t, "a blocked writer with a partly filled queue", func() bool {
		n := len(old.out)
		blocked := n > 0 && n == queued
		queued = n
		time.Sleep(300 * time.Millisecond)
		return blocked
	})
	if len(old.out) >= cap(old.out) {
		t.Fatalf("the outbound queue is full (%d frames), so the test does not cover a queue with free slots", len(old.out))
	}

	e.dp.ChannelLost(e.ch)
	e.ch.Close()
	lost := time.Now()
	select {
	case <-old.ctx.Done():
	case <-time.After(flush + 3*time.Second):
		t.Fatalf("the connection was not canceled %v after the channel loss", time.Since(lost))
	}
	if d := time.Since(lost); d < flush*8/10 {
		t.Fatalf("the connection was canceled after %v, before the %v flush deadline", d, flush)
	}
	// The grant of the new channel gets a new connection after the bounded
	// teardown.
	e.ch = newChannel(2)
	e.dp.ChannelOpened(e.ch)
	e.execGrant(e.execReq("/bin/true"))
	select {
	case <-e.conns:
	case <-time.After(teardownGrace + 5*time.Second):
		t.Fatal("no new relay connection for the live grant of the new channel")
	}
	if d := time.Since(lost); d > flush+teardownGrace+4*time.Second {
		t.Fatalf("the new connection took %v after the channel loss", d)
	}
}

// TestRelayChannelLossFlushesSessionEndBeforeClosingRequest checks the
// unblocked case: with a transport that reads, the queued session-end frame
// leaves before the request side closes, and the connection ends by that
// request close, well before the shutdown deadline.
func TestRelayChannelLossFlushesSessionEndBeforeClosingRequest(t *testing.T) {
	const flush = 10 * time.Second
	e := newRelayEnv(t, Timing{FlushDeadline: flush}, RelayLimits{})
	type event struct {
		frame *ownsandv1.DataFrame
		err   error
	}
	events := make(chan event, 4096)
	connected := make(chan struct{}, 1)
	e.cp.SetRelayHandler(func(ctx context.Context, _ string, stream *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		rc := &relayConn{t: t, ctx: ctx, frames: make(chan *ownsandv1.DataFrame, 4096), stream: stream}
		e.conns <- rc
		connected <- struct{}{}
		for {
			req, err := stream.Receive()
			if err != nil {
				events <- event{err: err}
				return nil
			}
			rc.frames <- req.GetFrame()
			events <- event{frame: req.GetFrame()}
		}
	})
	req := e.execReq("/bin/sh", "-c", "sleep 30")
	g := e.execGrant(req)
	rc := e.conn()
	<-connected
	e.openOK(rc, 1, g.GetGrantId())
	rc.send(datapath.MustNewFrame(1, e.sandbox, req))
	rc.nextOn(1)
	lost := time.Now()
	e.dp.ChannelLost(e.ch)
	e.ch.Close()
	var sawSessionEnd bool
	for {
		select {
		case ev := <-events:
			if ev.err == nil {
				if ev.frame.GetSessionEnd().GetReason() == ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST {
					sawSessionEnd = true
				}
				continue
			}
			if !errors.Is(ev.err, io.EOF) {
				t.Fatalf("the request side ended with %v, want a clean close", ev.err)
			}
			if !sawSessionEnd {
				t.Fatal("the request closed before the session-end frame arrived")
			}
			if d := time.Since(lost); d > flush/2 {
				t.Fatalf("the request closed %v after the channel loss, not at the end of the flush", d)
			}
			return
		case <-time.After(flush):
			t.Fatal("the request side never closed")
		}
	}
}

// relayAttempt is one relay connection of a scripted control plane.
type relayAttempt struct {
	started, ended time.Time
}

// scriptedRelay makes the control plane keep each relay connection open for
// the matching duration of lives (the last value repeats) and then fail it
// with an ordinary error, and records when each attempt started and ended.
// A negative duration fails the connection at once.
func scriptedRelay(e *relayEnv, lives ...time.Duration) func() []relayAttempt {
	var mu sync.Mutex
	var attempts []relayAttempt
	e.cp.SetRelayHandler(func(ctx context.Context, _ string, _ *connect.BidiStream[ownsandv1.ConnectRequest, ownsandv1.ConnectResponse]) error {
		mu.Lock()
		n := len(attempts)
		attempts = append(attempts, relayAttempt{started: time.Now()})
		mu.Unlock()
		life := lives[min(n, len(lives)-1)]
		if life > 0 {
			select {
			case <-time.After(life):
			case <-ctx.Done():
			}
		}
		mu.Lock()
		attempts[n].ended = time.Now()
		mu.Unlock()
		return connect.NewError(connect.CodeUnavailable, errors.New("relay unavailable"))
	})
	return func() []relayAttempt {
		mu.Lock()
		defer mu.Unlock()
		return append([]relayAttempt(nil), attempts...)
	}
}

// waitAttempts waits until the control plane saw n finished attempts.
func waitAttempts(t *testing.T, get func() []relayAttempt, n int) []relayAttempt {
	t.Helper()
	var got []relayAttempt
	waitFor(t, fmt.Sprintf("%d relay connection attempts", n), func() bool {
		got = get()
		return len(got) > n || (len(got) == n && !got[n-1].ended.IsZero())
	})
	return got
}

// TestRelayLongLivedConnectionRestartsBackoff checks that a connection that
// stayed open for the reset time and then ended with an ordinary error
// restarts the back-off at its first 200-millisecond step, also when earlier
// failures had raised it to ReconnectMax, and that the delay of a streak of
// short connections keeps growing.
func TestRelayLongLivedConnectionRestartsBackoff(t *testing.T) {
	// Three connections that fail at once raise the back-off to 800 ms; the
	// fourth lives longer than the reset time; the fifth is the redial.
	e := newRelayEnv(t, Timing{ReconnectMax: time.Second, ReconnectReset: 300 * time.Millisecond}, RelayLimits{})
	get := scriptedRelay(e, -1, -1, -1, 500*time.Millisecond, -1)
	e.execGrant(e.execReq("/bin/true"))
	at := waitAttempts(t, get, 5)
	gap := func(i int) time.Duration { return at[i+1].started.Sub(at[i].ended) }
	t.Logf("delays between attempts: %v, %v, %v (short connections), %v (after the long-lived one)", gap(0), gap(1), gap(2), gap(3))
	if gap(2) < 500*time.Millisecond {
		t.Fatalf("the delay after the third short connection was %v; the back-off did not grow", gap(2))
	}
	if g := gap(3); g > 400*time.Millisecond {
		t.Fatalf("the delay after a connection that lived 500 ms was %v, want at most the 200 ms first step", g)
	}
}

// TestRelayRepeatedLongLivedConnectionsStayAtFirstStep checks that every
// connection that lives for the reset time starts its own back-off.
func TestRelayRepeatedLongLivedConnectionsStayAtFirstStep(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: time.Second, ReconnectReset: 300 * time.Millisecond}, RelayLimits{})
	get := scriptedRelay(e, 400*time.Millisecond)
	e.execGrant(e.execReq("/bin/true"))
	at := waitAttempts(t, get, 4)
	t.Logf("delays after long-lived connections: %v, %v, %v", at[1].started.Sub(at[0].ended), at[2].started.Sub(at[1].ended), at[3].started.Sub(at[2].ended))
	for i := 0; i < 3; i++ {
		if g := at[i+1].started.Sub(at[i].ended); g > 400*time.Millisecond {
			t.Fatalf("the delay after long-lived connection %d was %v, want at most the 200 ms first step", i+1, g)
		}
	}
}

// TestRelayShortConnectionsKeepBackoffGrowing checks that connections that
// end before the reset time never restart the back-off, even when each of
// them lived for a while.
func TestRelayShortConnectionsKeepBackoffGrowing(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: time.Second, ReconnectReset: 300 * time.Millisecond}, RelayLimits{})
	get := scriptedRelay(e, 100*time.Millisecond)
	e.execGrant(e.execReq("/bin/true"))
	at := waitAttempts(t, get, 4)
	gap := func(i int) time.Duration { return at[i+1].started.Sub(at[i].ended) }
	if gap(2) < 500*time.Millisecond || gap(2) <= gap(0)+200*time.Millisecond {
		t.Fatalf("delays %v, %v, %v after short connections; the back-off did not keep growing", gap(0), gap(1), gap(2))
	}
}

// TestRelayCredentialFailureKeepsBackoffAtShortReset checks that a failure
// before any connection opened counts as no open time, so even a very short
// reset time never restarts the back-off of a failing credential request.
func TestRelayCredentialFailureKeepsBackoffAtShortReset(t *testing.T) {
	e := newRelayEnv(t, Timing{ReconnectMax: time.Second, ReconnectReset: 20 * time.Millisecond}, RelayLimits{})
	var attempts atomic.Int32
	hook := func(context.Context) error {
		attempts.Add(1)
		return errors.New("credential unavailable")
	}
	e.beforeAuth.Store(&hook)
	e.execGrant(e.execReq("/bin/true"))
	time.Sleep(1500 * time.Millisecond)
	// Back-off of 200, 400, 800 milliseconds allows about four attempts.
	if n := attempts.Load(); n < 1 || n > 6 {
		t.Fatalf("%d credential attempts in 1.5 seconds", n)
	}
}
