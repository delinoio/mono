package datapath

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cptest"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// fakeRelay is RelayAgentService.Connect on the fake control plane.
type fakeRelay struct {
	cp    *cptest.Server
	conns chan *fakeConn
	mu    sync.Mutex
	creds []string
}

type fakeConn struct {
	stream *connect.BidiStream[ownsandv1.DataFrame, ownsandv1.DataFrame]
	in     chan *ownsandv1.DataFrame
	cancel context.CancelFunc
	done   chan struct{}
	wmu    sync.Mutex
}

func (c *fakeConn) send(f *ownsandv1.DataFrame) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_ = c.stream.Send(f)
}

func (f *fakeRelay) Connect(ctx context.Context, stream *connect.BidiStream[ownsandv1.DataFrame, ownsandv1.DataFrame]) error {
	cred := cptest.BearerCredential(stream.RequestHeader())
	if !f.cp.ValidCredential(cred) {
		return connect.NewError(connect.CodeUnauthenticated, errors.New("invalid credential"))
	}
	f.mu.Lock()
	f.creds = append(f.creds, cred)
	f.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	c := &fakeConn{stream: stream, in: make(chan *ownsandv1.DataFrame, 4096), cancel: cancel, done: make(chan struct{})}
	defer close(c.done)
	go func() {
		for {
			fr, err := stream.Receive()
			if err != nil {
				cancel()
				return
			}
			c.in <- fr
		}
	}()
	f.conns <- c
	<-ctx.Done()
	return nil
}

type relayHarness struct {
	t     *testing.T
	sb    *testSandbox
	cp    *cptest.Server
	relay *fakeRelay
	dp    *DataPath
	envs  *envLog
	gen   uint64
}

func newRelayHarness(t *testing.T, lim *RelayLimits) *relayHarness {
	t.Helper()
	sb := newSandbox(t)
	fr := &fakeRelay{conns: make(chan *fakeConn, 16)}
	cp := cptest.New(t, func(s *cptest.Server, mux *http.ServeMux) {
		fr.cp = s
		path, h := ownsandv1connect.NewRelayAgentServiceHandler(fr)
		mux.Handle(path, h)
	})
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	deviceID := sdkdp.NewID()
	cp.AddDevice(deviceID, priv.Public().(ed25519.PublicKey))
	client, err := controlplane.New(cp.URL)
	if err != nil {
		t.Fatal(err)
	}
	auth := func(ctx context.Context) (string, error) {
		_, s, err := client.Authenticate(ctx, deviceID, priv, "epoch")
		if err != nil {
			return "", err
		}
		return s.Credential, nil
	}
	dp, err := New(Config{Sandboxes: sb.mgr, Logger: testLog, Origin: cp.URL, Authenticate: auth, Limits: lim})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(dp.Close)
	h := &relayHarness{t: t, sb: sb, cp: cp, relay: fr, dp: dp, envs: &envLog{}, gen: 1}
	dp.Connected(1, h.envs.send)
	return h
}

func (h *relayHarness) grant(kind ownsandv1.RelayOperationKind, opID string, digest []byte) *ownsandv1.RelayGrant {
	g := relayGrant(h.sb.id)
	g.OperationKind, g.OperationId, g.RequestDigest = kind, opID, digest
	h.dp.HandleRelayGrant(h.gen, g)
	return g
}

func (h *relayHarness) conn() *fakeConn {
	h.t.Helper()
	select {
	case c := <-h.relay.conns:
		return c
	case <-time.After(10 * time.Second):
		h.t.Fatal("the agent did not open a relay connection")
		return nil
	}
}

// next returns the next frame of a stream, skipping connection frames.
func next(t *testing.T, c *fakeConn, stream uint64) *ownsandv1.DataFrame {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case f := <-c.in:
			if f.GetStreamId() == stream {
				return f
			}
		case <-timeout:
			t.Fatalf("no frame on stream %d", stream)
			return nil
		}
	}
}

func openFrame(id uint64, sandboxID, grantID string, window int64) *ownsandv1.DataFrame {
	return sdkdp.NewFrame(id, sandboxID, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayStreamOpen{RelayStreamOpen: &ownsandv1.RelayStreamOpen{GrantId: grantID, InitialWindow: window}}
	})
}

func TestRelayConnectionOpensAndServesExec(t *testing.T) {
	h := newRelayHarness(t, nil)
	req := newExec("/bin/sh", "-c", "echo relayed")
	req.SandboxId = h.sb.id
	g := h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.ExecId, sdkdp.ExecDigest(req))
	c := h.conn()
	// The connection starts with a keepalive and the supported kinds.
	first, second := <-c.in, <-c.in
	if first.GetKeepalive() == nil || second.GetSupportedKinds() == nil || len(second.GetSupportedKinds().GetKinds()) != len(sdkdp.SupportedKinds()) {
		t.Fatalf("connection start: %v %v", first, second)
	}
	c.send(openFrame(1, h.sb.id, g.GetGrantId(), 4<<20))
	if w := next(t, c, 1).GetRelayWindowUpdate(); w.GetAddedBytes() != DefaultRelayLimits().StreamWindow {
		t.Fatalf("first window %v", w)
	}
	c.send(execFrame(h.sb.id, req))
	sawAccepted, out := false, ""
	for {
		f := next(t, c, 1)
		if f.GetRequestAccepted() != nil {
			sawAccepted = true
			continue
		}
		if f.GetRelayWindowUpdate() != nil {
			continue
		}
		if o := f.GetExecEvent().GetOutput(); o != nil {
			out += string(o.GetData())
		}
		if f.GetExecEvent().GetExited() != nil {
			break
		}
	}
	if !sawAccepted || out != "relayed\n" {
		t.Fatalf("accepted %v, output %q", sawAccepted, out)
	}
	// The operation finished and its stream ended: the grant is released.
	deadline := time.Now().Add(5 * time.Second)
	for len(h.envs.releases()) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if rel := h.envs.releases(); len(rel) != 1 || rel[0] != g.GetGrantId() {
		t.Fatalf("releases %v", rel)
	}
}

func TestRelayStreamChecks(t *testing.T) {
	lim := DefaultRelayLimits()
	lim.MaxStreams = 2
	h := newRelayHarness(t, &lim)
	req := newExec("/bin/sh", "-c", "sleep 2")
	req.SandboxId = h.sb.id
	g := h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.ExecId, sdkdp.ExecDigest(req))
	c := h.conn()
	// An unknown grant ID gets GRANT_INVALID.
	c.send(openFrame(1, h.sb.id, sdkdp.NewID(), 4<<20))
	if se := next(t, c, 1).GetSessionEnd(); se.GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("unknown grant: %v", se)
	}
	// A request that does not match the grant gets permission_denied.
	c.send(openFrame(2, h.sb.id, g.GetGrantId(), 4<<20))
	next(t, c, 2) // window
	other := newExec("/bin/true")
	other.ExecId = req.ExecId
	c.send(on(2, execFrame(h.sb.id, other)))
	if next(t, c, 2).GetRelayWindowUpdate() == nil {
		t.Fatal("no credit returned for the request")
	}
	if e := next(t, c, 2).GetError(); e.GetCode() != "permission_denied" {
		t.Fatalf("mismatched request: %v", e)
	}
	// The mismatch left no operation, so the agent released the grant.
	c.send(openFrame(6, h.sb.id, g.GetGrantId(), 4<<20))
	if se := next(t, c, 6).GetSessionEnd(); se.GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		t.Fatalf("released grant: %v", se)
	}
	g = h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, req.ExecId, nil)
	// Stream caps: two open streams, the third gets resource_exhausted.
	c.send(openFrame(7, h.sb.id, g.GetGrantId(), 4<<20))
	c.send(openFrame(8, h.sb.id, g.GetGrantId(), 4<<20))
	next(t, c, 7)
	next(t, c, 8)
	c.send(openFrame(9, h.sb.id, g.GetGrantId(), 4<<20))
	if e := next(t, c, 9).GetError(); e.GetCode() != "resource_exhausted" {
		t.Fatalf("stream cap: %v", e)
	}
	// A reused stream ID is dropped.
	c.send(openFrame(7, h.sb.id, g.GetGrantId(), 4<<20))
	// Revocation ends the open streams of the grant.
	h.dp.HandleRevocation(h.gen, &ownsandv1.SessionRevocation{RevocationId: "r1", Target: &ownsandv1.SessionRevocation_GrantId{GrantId: g.GetGrantId()}})
	got := map[uint64]ownsandv1.SessionEndReason{}
	timeout := time.After(10 * time.Second)
	for len(got) < 2 {
		select {
		case f := <-c.in:
			if se := f.GetSessionEnd(); se != nil {
				got[f.GetStreamId()] = se.GetReason()
			}
		case <-timeout:
			t.Fatalf("session ends after revocation: %v", got)
		}
	}
	for _, id := range []uint64{7, 8} {
		if got[id] != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
			t.Fatalf("stream %d after revocation: %v", id, got[id])
		}
	}
}

func TestRelayCreditWindow(t *testing.T) {
	h := newRelayHarness(t, nil)
	req := newExec("/bin/sh", "-c", "i=0; while [ $i -lt 50 ]; do echo line-$i; i=$((i+1)); sleep 0.01; done")
	req.SandboxId = h.sb.id
	g := h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC, req.ExecId, sdkdp.ExecDigest(req))
	c := h.conn()
	const window = 200
	c.send(openFrame(1, h.sb.id, g.GetGrantId(), window))
	next(t, c, 1)
	c.send(execFrame(h.sb.id, req))
	var used int64
	idle := time.After(1500 * time.Millisecond)
loop:
	for {
		select {
		case f := <-c.in:
			if f.GetStreamId() != 1 || !sdkdp.PayloadUsesCredit(f) {
				continue
			}
			used += sdkdp.FrameSize(f)
			if used > window {
				t.Fatalf("the agent sent %d bytes beyond a %d-byte window", used, window)
			}
		case <-idle:
			break loop
		}
	}
	if used == 0 {
		t.Fatal("nothing was sent within the window")
	}
	c.send(sdkdp.NewFrame(1, h.sb.id, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: 1 << 20}}
	}))
	for {
		if next(t, c, 1).GetExecEvent().GetExited() != nil {
			return
		}
	}
}

func TestRelayReconnectsWithNewCredentialAndClosesWithChannel(t *testing.T) {
	lim := DefaultRelayLimits()
	lim.ReceiveTimeout = 600 * time.Millisecond
	lim.KeepaliveAfter = time.Hour
	h := newRelayHarness(t, &lim)
	g := h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, sdkdp.NewID(), nil)
	c1 := h.conn()
	// The fake sends nothing: the receive timeout makes the agent redial
	// while the grant is live, with a new device session credential.
	c2 := h.conn()
	go c2.pump()
	h.relay.mu.Lock()
	creds := append([]string(nil), h.relay.creds...)
	h.relay.mu.Unlock()
	if len(creds) < 2 || creds[0] == creds[1] {
		t.Fatalf("credentials %v", len(creds))
	}
	select {
	case <-c1.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the lost connection stayed open")
	}
	// An open stream ends with AGENT_CHANNEL_LOST when the agent channel
	// closes, and the relay connection closes.
	c2.send(openFrame(1, h.sb.id, g.GetGrantId(), 4<<20))
	next(t, c2, 1)
	h.dp.Disconnected(h.gen)
	if se := next(t, c2, 1).GetSessionEnd(); se.GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_AGENT_CHANNEL_LOST {
		t.Fatalf("after channel loss: %v", se)
	}
	select {
	case <-c2.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the relay connection outlived its agent channel")
	}
	select {
	case <-h.relay.conns:
		t.Fatal("the agent dialed without a live grant")
	case <-time.After(time.Second):
	}
}

func TestRelayEchoProbeBeforeEarlyReconnect(t *testing.T) {
	lim := DefaultRelayLimits()
	lim.ProbeAfter = 300 * time.Millisecond
	lim.ProbeWait = 300 * time.Millisecond
	lim.KeepaliveAfter = time.Hour
	h := newRelayHarness(t, &lim)
	h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, sdkdp.NewID(), nil)
	c1 := h.conn()
	<-c1.in
	<-c1.in
	time.Sleep(400 * time.Millisecond)
	// A new grant on an idle connection sends an echo probe; an answer keeps
	// the connection.
	h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, sdkdp.NewID(), nil)
	if k := (<-c1.in).GetKeepalive(); !k.GetEchoRequested() {
		t.Fatal("no echo probe")
	}
	c1.send(keepaliveFrame(false))
	select {
	case <-h.relay.conns:
		t.Fatal("the agent reconnected although the probe was answered")
	case <-time.After(700 * time.Millisecond):
	}
	// An unanswered probe makes the agent dial again at once.
	h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, sdkdp.NewID(), nil)
	if k := (<-c1.in).GetKeepalive(); !k.GetEchoRequested() {
		t.Fatal("no second echo probe")
	}
	h.conn()
}

func TestRelayIdleClose(t *testing.T) {
	lim := DefaultRelayLimits()
	lim.IdleClose = 300 * time.Millisecond
	h := newRelayHarness(t, &lim)
	g := h.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, sdkdp.NewID(), nil)
	c := h.conn()
	h.dp.HandleRevocation(h.gen, &ownsandv1.SessionRevocation{RevocationId: "r", Target: &ownsandv1.SessionRevocation_GrantId{GrantId: g.GetGrantId()}})
	select {
	case <-c.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the connection stayed open without a live grant")
	}
}

// on moves a frame to another stream.
func on(id uint64, f *ownsandv1.DataFrame) *ownsandv1.DataFrame {
	f.StreamId = id
	return f
}

// pump sends keepalives so the agent's receive timeout does not end the
// connection.
func (c *fakeConn) pump() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-t.C:
			c.send(keepaliveFrame(false))
		}
	}
}

var testLog = quiet
