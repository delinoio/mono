// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/pion/webrtc/v4"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const (
	relayTransport = ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY
	p2pTransport   = ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER
)

// execResult carries the outcome of an Exec started in the background.
type execResult struct {
	res *ExecResult
	rec *recorder
	err error
	at  time.Time
}

// startExec runs one Exec in the background.
func startExec(ctx context.Context, s *Sandbox) <-chan execResult {
	out := make(chan execResult, 1)
	go func() {
		r := &recorder{}
		res, err := s.Exec(ctx, ExecParams{Command: []string{"run"}, OnEvent: r.on})
		out <- execResult{res: res, rec: r, err: err, at: time.Now()}
	}()
	return out
}

func waitExec(t *testing.T, ch <-chan execResult) execResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(20 * time.Second):
		t.Fatal("exec did not return")
		return execResult{}
	}
}

// waitGate waits until a request reached a closed gate of kind.
func waitGate(t *testing.T, cp *fakeCP, kind string) {
	t.Helper()
	select {
	case got := <-cp.gateHits:
		if got != kind {
			t.Fatalf("request reached the %q gate, want %q", got, kind)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("no request reached the %q gate", kind)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func answererClosed(cp *fakeCP, i int) bool {
	cp.mu.Lock()
	defer cp.mu.Unlock()
	if i >= len(cp.answerers) {
		return false
	}
	switch cp.answerers[i].ConnectionState() {
	case webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected, webrtc.PeerConnectionStateFailed:
		return true
	}
	return false
}

func (s *Sandbox) installedSession() *p2pSession {
	s.dp.mu.Lock()
	defer s.dp.mu.Unlock()
	return s.dp.sess
}

func TestCloseDuringSetupReleasesTheSetup(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.iceGate = make(chan struct{})
	cp.gateHits = make(chan string, 4)
	s := cp.handle(t, cp.client(t, TransportAuto))

	ch := startExec(context.Background(), s)
	waitGate(t, cp, "ice")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	r := waitExec(t, ch)
	if !errors.Is(r.err, errHandleClosed) {
		t.Fatalf("exec after Close during setup: %v", r.err)
	}
	waitFor(t, "the held request to end", func() bool { return cp.gateCanceledCount() == 1 })
	close(cp.iceGate)
	if s.installedSession() != nil {
		t.Fatal("a session was installed on a closed handle")
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	// A closed handle rejects a later peer-to-peer operation before it calls
	// the control plane.
	before := cp.iceCallCount()
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
	if !errors.Is(err, errHandleClosed) {
		t.Fatalf("exec on a closed handle: %v", err)
	}
	if cp.iceCallCount() != before || cp.openSessionCount() != 0 {
		t.Fatal("a closed handle called the control plane")
	}
}

func TestCloseAfterSetupBeforeInstallClosesTheSession(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	c := cp.client(t, TransportAuto)
	s := cp.handle(t, c)
	c.p2p.setupDone = func() {
		// The setup produced a live session; close the handle before the
		// handle installs it.
		_ = s.Close()
	}
	_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
	if !errors.Is(err, errHandleClosed) {
		t.Fatalf("exec: %v", err)
	}
	if s.installedSession() != nil {
		t.Fatal("a session was installed on a closed handle")
	}
	if cp.openSessionCount() != 1 {
		t.Fatalf("%d OpenSession calls, want 1", cp.openSessionCount())
	}
	waitFor(t, "the session peer to see the close", func() bool { return answererClosed(cp, 0) })
}

func TestCloseClosesTheInstalledSession(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	s := cp.handle(t, cp.client(t, TransportAuto))
	res, r := runExec(t, s)
	checkLines(t, res, r, 3)
	if s.installedSession() == nil {
		t.Fatal("no session installed")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.installedSession() != nil {
		t.Fatal("the session stayed installed after Close")
	}
	waitFor(t, "the session peer to see the close", func() bool { return answererClosed(cp, 0) })
	if err := s.Close(); err != nil {
		t.Fatalf("repeated Close: %v", err)
	}
	// The relay-only transport never opens a session, so a closed handle
	// still serves relay operations.
	rs := cp.handle(t, cp.client(t, TransportRelay))
	_ = rs.Close()
	res, r = runExec(t, rs)
	checkLines(t, res, r, 3)
}

func TestWaitingOperationCancelsWithoutEndingTheSetup(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.iceGate = make(chan struct{})
	cp.gateHits = make(chan string, 4)
	s := cp.handle(t, cp.client(t, TransportAuto))

	a := startExec(context.Background(), s)
	waitGate(t, cp, "ice")
	// Waiter C keeps waiting; waiter B cancels.
	c := startExec(context.Background(), s)
	bctx, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	b := startExec(bctx, s)
	time.Sleep(100 * time.Millisecond)
	canceledAt := time.Now()
	cancelB()
	rb := waitExec(t, b)
	if !errors.Is(rb.err, context.Canceled) {
		t.Fatalf("waiter error: %v", rb.err)
	}
	if d := rb.at.Sub(canceledAt); d > time.Second {
		t.Fatalf("waiter returned %v after its cancellation", d)
	}
	select {
	case <-a:
		t.Fatal("the opening operation ended with the waiter")
	case <-c:
		t.Fatal("another waiter ended with the canceled waiter")
	default:
	}
	if cp.gateCanceledCount() != 0 {
		t.Fatal("the waiter's cancellation canceled the setup")
	}
	close(cp.iceGate)
	for name, ch := range map[string]<-chan execResult{"opener": a, "waiter": c} {
		r := waitExec(t, ch)
		if r.err != nil {
			t.Fatalf("%s: %v", name, r.err)
		}
		checkLines(t, r.res, r.rec, 3)
		if r.res.Transport != p2pTransport {
			t.Fatalf("%s transport %v", name, r.res.Transport)
		}
	}
	if cp.iceCallCount() != 1 || cp.openSessionCount() != 1 {
		t.Fatalf("%d GetIceServers and %d OpenSession calls, want one setup", cp.iceCallCount(), cp.openSessionCount())
	}
}

// TestWaiterRunsTheSetupWhenTheOpenerEnds records the chosen behavior: when
// the operation that runs the setup ends first, a waiting operation runs the
// setup itself.
func TestWaiterRunsTheSetupWhenTheOpenerEnds(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.iceGate = make(chan struct{})
	cp.gateHits = make(chan string, 4)
	s := cp.handle(t, cp.client(t, TransportAuto))

	actx, cancelA := context.WithCancel(context.Background())
	defer cancelA()
	a := startExec(actx, s)
	waitGate(t, cp, "ice")
	w := startExec(context.Background(), s)
	time.Sleep(100 * time.Millisecond)
	cancelA()
	if ra := waitExec(t, a); !errors.Is(ra.err, context.Canceled) {
		t.Fatalf("opener error: %v", ra.err)
	}
	waitGate(t, cp, "ice") // the waiter became the opener
	close(cp.iceGate)
	rw := waitExec(t, w)
	if rw.err != nil {
		t.Fatal(rw.err)
	}
	checkLines(t, rw.res, rw.rec, 3)
	if rw.res.Transport != p2pTransport || cp.iceCallCount() != 2 {
		t.Fatalf("transport %v after %d GetIceServers calls", rw.res.Transport, cp.iceCallCount())
	}
}

// neverRepliesSTUN returns a STUN URL on a loopback UDP socket that reads
// requests and never answers them, so ICE gathering waits for the server.
func neverRepliesSTUN(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, err := pc.ReadFrom(buf); err != nil {
				return
			}
		}
	}()
	return "stun:" + pc.LocalAddr().String()
}

// stalledStage names a stage of the session setup that stalls.
type stalledStage string

const (
	stageGetIceServers stalledStage = "get-ice-servers"
	stageGathering     stalledStage = "gathering"
	stageOpenSession   stalledStage = "open-session"
	stageChannelOpen   stalledStage = "channel-open"
)

// stallSetup configures the fake so that the setup stalls in stage.
func stallSetup(t *testing.T, cp *fakeCP, stage stalledStage) {
	t.Helper()
	cp.gateHits = make(chan string, 8)
	switch stage {
	case stageGetIceServers:
		cp.iceGate = make(chan struct{})
	case stageOpenSession:
		cp.openGate = make(chan struct{})
	case stageGathering:
		cp.iceServers = []string{neverRepliesSTUN(t)}
	case stageChannelOpen:
		cp.iceFail = true
	}
}

func TestStalledSetupFallsBackToRelayWithinTheSetupWindow(t *testing.T) {
	for _, stage := range []stalledStage{stageGetIceServers, stageGathering, stageOpenSession, stageChannelOpen} {
		t.Run(string(stage), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			stallSetup(t, cp, stage)
			c := cp.client(t, TransportAuto)
			c.timing.p2pOpenTimeout = 300 * time.Millisecond
			s := cp.handle(t, c)

			start := time.Now()
			// No caller deadline: only the setup window ends the stall.
			r := waitExec(t, startExec(context.Background(), s))
			if r.err != nil {
				t.Fatal(r.err)
			}
			checkLines(t, r.res, r.rec, 3)
			if r.res.Transport != relayTransport || s.Transport() != relayTransport {
				t.Fatalf("transport %v", r.res.Transport)
			}
			if d := r.at.Sub(start); d > 5*time.Second {
				t.Fatalf("exec took %v with a 300ms setup window", d)
			}
			if len(cp.authRequests()) == 0 {
				t.Fatal("the relay was not used")
			}
			switch stage {
			case stageGetIceServers, stageOpenSession:
				waitFor(t, "the held request to be canceled", func() bool { return cp.gateCanceledCount() == 1 })
			case stageGathering:
				if cp.openSessionCount() != 0 {
					t.Fatal("OpenSession was called although gathering did not complete")
				}
			}
			// The handle stays on the relay: no new setup.
			ice := cp.iceCallCount()
			runExec(t, s)
			if cp.iceCallCount() != ice {
				t.Fatal("a second setup started after the fallback")
			}
		})
	}
}

func TestStalledSetupOnForcedPeerToPeerReturnsTheSetupError(t *testing.T) {
	for _, stage := range []stalledStage{stageGetIceServers, stageGathering, stageOpenSession} {
		t.Run(string(stage), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			stallSetup(t, cp, stage)
			c := cp.client(t, TransportPeerToPeer)
			c.timing.p2pOpenTimeout = 300 * time.Millisecond
			s := cp.handle(t, c)

			r := waitExec(t, startExec(context.Background(), s))
			var openErr *errP2POpen
			if !errors.As(r.err, &openErr) || !errors.Is(r.err, errSetupTimeout) {
				t.Fatalf("exec error: %v", r.err)
			}
			if len(cp.authRequests()) != 0 {
				t.Fatal("AuthorizeRelay called with the transport forced to peer-to-peer")
			}
		})
	}
}

func TestCallerDeadlineEndsStalledSetupWithoutRelayRecovery(t *testing.T) {
	for _, stage := range []stalledStage{stageGetIceServers, stageGathering, stageOpenSession} {
		t.Run(string(stage), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			stallSetup(t, cp, stage)
			c := cp.client(t, TransportAuto)
			c.timing.p2pOpenTimeout = 5 * time.Second
			s := cp.handle(t, c)

			ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
			defer cancel()
			start := time.Now()
			r := waitExec(t, startExec(ctx, s))
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("exec error: %v", r.err)
			}
			if d := r.at.Sub(start); d > 3*time.Second {
				t.Fatalf("exec took %v", d)
			}
			if len(cp.authRequests()) != 0 {
				t.Fatal("relay recovery started after the caller's deadline")
			}
			if s.Transport() == relayTransport {
				t.Fatal("the handle moved to the relay on a caller deadline")
			}
		})
	}
}

// A control plane that applies the caller's deadline (Connect sends the time
// left as a timeout header) can answer deadline_exceeded slightly before the
// caller's own timer fires. The test makes that order deterministic: the held
// request fails with deadline_exceeded at once, while the caller's context
// still has time left and the skew is larger than that time.
func TestControlPlaneDeadlineStatusBeforeCallerTimerIsTheCallerDeadline(t *testing.T) {
	type tc struct {
		stage stalledStage
		code  connect.Code
	}
	var cases []tc
	for _, stage := range []stalledStage{stageGetIceServers, stageOpenSession} {
		for _, code := range []connect.Code{connect.CodeDeadlineExceeded, connect.CodeUnavailable} {
			cases = append(cases, tc{stage, code})
		}
	}
	for _, tt := range cases {
		stage := tt.stage
		t.Run(string(stage)+"/"+tt.code.String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			stallSetup(t, cp, stage)
			cp.gateErr = connect.NewError(tt.code, errors.New("control plane ended the request"))
			c := cp.client(t, TransportAuto)
			c.timing.p2pOpenTimeout = 5 * time.Second
			c.timing.callerDeadlineSkew = 10 * time.Second
			s := cp.handle(t, c)

			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			r := waitExec(t, startExec(ctx, s))
			if !errors.Is(r.err, context.DeadlineExceeded) {
				t.Fatalf("exec error: %v", r.err)
			}
			if len(cp.authRequests()) != 0 {
				t.Fatal("relay recovery started after the caller's deadline")
			}
			if s.Transport() == relayTransport {
				t.Fatal("the handle moved to the relay on a caller deadline")
			}
		})
	}
}

// A deadline_exceeded status without a nearby caller deadline is a control
// plane failure: automatic routing uses the relay.
func TestControlPlaneDeadlineStatusWithoutCallerDeadlineFallsBackToRelay(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	stallSetup(t, cp, stageGetIceServers)
	cp.gateErr = connect.NewError(connect.CodeDeadlineExceeded, errors.New("server deadline"))
	c := cp.client(t, TransportAuto)
	s := cp.handle(t, c)

	r := waitExec(t, startExec(context.Background(), s))
	if r.err != nil {
		t.Fatal(r.err)
	}
	if r.res.Transport != relayTransport || len(cp.authRequests()) == 0 {
		t.Fatalf("transport %v", r.res.Transport)
	}
}

// logBuffer is a goroutine-safe log sink for the logger of a test client.
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// captureLogs makes the client write its debug logs to the returned buffer.
func captureLogs(c *Client) *logBuffer {
	buf := &logBuffer{}
	c.logger = slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf
}

// heldOp is one operation that is in flight on the peer-to-peer session
// while the agent holds its request.
type heldOp struct {
	name string
	// start begins the operation in the background and returns when the
	// agent holds the request, with the channel of the operation's error.
	start func(t *testing.T, cp *fakeCP, s *Sandbox) <-chan error
}

var heldOps = []heldOp{
	{"exec", func(t *testing.T, cp *fakeCP, s *Sandbox) <-chan error {
		release := make(chan struct{})
		t.Cleanup(func() { close(release) })
		cp.agent.run = func(_ []string, e *fakeExec) {
			<-release
			e.finish(exitedEvent(0))
		}
		out := make(chan error, 1)
		go func() {
			_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
			out <- err
		}()
		waitFor(t, "the exec to run on the agent", func() bool { return cp.agent.totalRuns() == 1 })
		return out
	}},
	{"read", func(t *testing.T, cp *fakeCP, s *Sandbox) <-chan error {
		cp.agent.setFile("/f", []byte("data"))
		cp.agentGate = make(chan struct{})
		cp.agentHits = make(chan string, 4)
		out := make(chan error, 1)
		go func() {
			_, err := s.ReadFileBytes(context.Background(), "/f")
			out <- err
		}()
		if got := <-cp.agentHits; got != "read" {
			t.Fatalf("agent gate %q", got)
		}
		return out
	}},
	{"write", func(t *testing.T, cp *fakeCP, s *Sandbox) <-chan error {
		cp.agentGate = make(chan struct{})
		cp.agentHits = make(chan string, 4)
		out := make(chan error, 1)
		go func() { out <- s.WriteFileBytes(context.Background(), "/f", []byte("data"), WriteOptions{}) }()
		if got := <-cp.agentHits; got != "write" {
			t.Fatalf("agent gate %q", got)
		}
		return out
	}},
}

func waitOpErr(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("operation did not return")
		return nil
	}
}

// closeCases lists the ways that the caller ends a handle: Close and a
// successful Delete both close the data path.
var closeCases = []struct {
	name  string
	close func(t *testing.T, s *Sandbox)
}{
	{"Close", func(t *testing.T, s *Sandbox) {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
	}},
	{"Delete", func(t *testing.T, s *Sandbox) {
		if _, err := s.Delete(context.Background()); err != nil {
			t.Fatalf("delete: %v", err)
		}
	}},
}

// TestCloseDuringOperationKeepsThePeerToPeerTransport checks that a close
// that the caller starts while an operation runs on the session ends the
// operation with errHandleClosed. The close is not a channel failure, so the
// handle stays on peer-to-peer, asks for no relay grant and logs no fall back.
func TestCloseDuringOperationKeepsThePeerToPeerTransport(t *testing.T) {
	for _, op := range heldOps {
		for _, cc := range closeCases {
			t.Run(op.name+"/"+cc.name, func(t *testing.T) {
				cp := newFakeCP(t)
				c := cp.client(t, TransportAuto)
				logs := captureLogs(c)
				s := cp.handle(t, c)
				ch := op.start(t, cp, s)
				if s.Transport() != p2pTransport {
					t.Fatalf("transport %v before the close, want peer-to-peer", s.Transport())
				}
				cc.close(t, s)
				if err := waitOpErr(t, ch); !errors.Is(err, errHandleClosed) {
					t.Fatalf("operation after the close: %v, want errHandleClosed", err)
				}
				if n := len(cp.authRequests()); n != 0 {
					t.Fatalf("%d AuthorizeRelay calls after the close, want 0", n)
				}
				if got := s.Transport(); got != p2pTransport {
					t.Fatalf("transport %v after the close, want peer-to-peer", got)
				}
				s.dp.mu.Lock()
				relay := s.dp.relay
				s.dp.mu.Unlock()
				if relay {
					t.Fatal("the handle moved to the relay after a caller close")
				}
				if got := logs.String(); strings.Contains(got, "falls back to the relay") {
					t.Fatalf("a caller close logged a relay fall back:\n%s", got)
				}

				// Later operations fail with errHandleClosed and call no control
				// plane method.
				ice, open := cp.iceCallCount(), cp.openSessionCount()
				_, err := s.Exec(context.Background(), ExecParams{Command: []string{"run"}})
				if !errors.Is(err, errHandleClosed) {
					t.Fatalf("exec after the close: %v", err)
				}
				if _, err := s.ReadFileBytes(context.Background(), "/f"); !errors.Is(err, errHandleClosed) {
					t.Fatalf("read after the close: %v", err)
				}
				if err := s.WriteFileBytes(context.Background(), "/g", []byte("x"), WriteOptions{}); !errors.Is(err, errHandleClosed) {
					t.Fatalf("write after the close: %v", err)
				}
				if cp.iceCallCount() != ice || cp.openSessionCount() != open {
					t.Fatal("an operation on a closed handle called the control plane")
				}
				if n := len(cp.authRequests()); n != 0 {
					t.Fatalf("%d AuthorizeRelay calls by later operations, want 0", n)
				}
			})
		}
	}
}

// TestCloseDuringOperationOnForcedPeerToPeer checks that forced
// peer-to-peer ends the operation with errHandleClosed too.
func TestCloseDuringOperationOnForcedPeerToPeer(t *testing.T) {
	for _, op := range heldOps {
		t.Run(op.name, func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			ch := op.start(t, cp, s)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if err := waitOpErr(t, ch); !errors.Is(err, errHandleClosed) {
				t.Fatalf("operation after the close: %v, want errHandleClosed", err)
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls, want 0", n)
			}
			if got := s.Transport(); got != p2pTransport {
				t.Fatalf("transport %v, want peer-to-peer", got)
			}
		})
	}
}

// execOnce runs one exec on s with a bounded context.
func execOnce(t *testing.T, s *Sandbox) (*ExecResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return s.Exec(ctx, ExecParams{Command: []string{"run"}})
}

func (s *Sandbox) usesRelay() bool {
	s.dp.mu.Lock()
	defer s.dp.mu.Unlock()
	return s.dp.relay
}

// failedPrecondition is the status of a control plane request that the sandbox
// state does not allow.
func failedPrecondition() error {
	return connect.NewError(connect.CodeFailedPrecondition, errors.New("sandbox not running"))
}

// TestGetIceServersFailedPreconditionMovesToRelay checks that a
// failed_precondition from GetIceServers has no sandbox-state meaning: the
// operation succeeds on the relay and the handle moves to the relay, so a
// later operation does not call GetIceServers again.
func TestGetIceServersFailedPreconditionMovesToRelay(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.iceGate = make(chan struct{})
	cp.gateErr = failedPrecondition()
	c := cp.client(t, TransportAuto)
	logs := captureLogs(c)
	s := cp.handle(t, c)

	for i := range 2 {
		res, err := execOnce(t, s)
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		if res.Transport != relayTransport {
			t.Fatalf("exec %d transport %v, want relay", i, res.Transport)
		}
	}
	if !s.usesRelay() || s.Transport() != relayTransport {
		t.Fatalf("handle did not move to the relay: transport %v", s.Transport())
	}
	if n := cp.iceCallCount(); n != 1 {
		t.Fatalf("%d GetIceServers calls, want 1: the second operation must use the relay directly", n)
	}
	if n := cp.openSessionCount(); n != 0 {
		t.Fatalf("%d OpenSession calls, want 0", n)
	}
	if got := logs.String(); !strings.Contains(got, "falls back to the relay") || strings.Contains(got, "using the relay once") {
		t.Fatalf("unexpected logs:\n%s", got)
	}
}

// TestOpenSessionFailedPreconditionUsesRelayOnce checks that a
// failed_precondition from OpenSession (a sandbox that is not RUNNING) still
// uses the relay for that attempt only: the next operation sets up again.
func TestOpenSessionFailedPreconditionUsesRelayOnce(t *testing.T) {
	cp := newFakeCP(t)
	cp.agent.run = lineRun(3, 0)
	cp.mu.Lock()
	cp.openErr = failedPrecondition()
	cp.mu.Unlock()
	s := cp.handle(t, cp.client(t, TransportAuto))

	for i := range 2 {
		res, err := execOnce(t, s)
		if err != nil {
			t.Fatalf("exec %d: %v", i, err)
		}
		if res.Transport != relayTransport {
			t.Fatalf("exec %d transport %v, want relay", i, res.Transport)
		}
	}
	if s.usesRelay() {
		t.Fatal("the handle moved to the relay on an OpenSession failed_precondition")
	}
	if ice, open := cp.iceCallCount(), cp.openSessionCount(); ice != 2 || open != 2 {
		t.Fatalf("%d GetIceServers and %d OpenSession calls, want 2 and 2", ice, open)
	}
}

// TestForcedPeerToPeerFailedPreconditionReturnsTheError checks that a forced
// peer-to-peer handle returns the failed_precondition of either setup step
// with its status code and asks for no relay grant.
func TestForcedPeerToPeerFailedPreconditionReturnsTheError(t *testing.T) {
	for _, stage := range []stalledStage{stageGetIceServers, stageOpenSession} {
		t.Run(string(stage), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			if stage == stageGetIceServers {
				cp.iceGate = make(chan struct{})
				cp.gateErr = failedPrecondition()
			} else {
				cp.mu.Lock()
				cp.openErr = failedPrecondition()
				cp.mu.Unlock()
			}
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			_, err := execOnce(t, s)
			if got := connect.CodeOf(err); got != connect.CodeFailedPrecondition {
				t.Fatalf("code %v, want failed_precondition (err %v)", got, err)
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls, want 0", n)
			}
		})
	}
}

// TestGetIceServersTerminalErrorsEndTheOperation checks that the errors that
// are terminal for OpenSession are terminal for GetIceServers too.
func TestGetIceServersTerminalErrorsEndTheOperation(t *testing.T) {
	billing := billingErr(ownsandv1.BillingReason_BILLING_REASON_SUSPENDED)
	for _, err := range []error{
		connect.NewError(connect.CodeUnauthenticated, errors.New("bad credential")),
		connect.NewError(connect.CodePermissionDenied, errors.New("denied")),
		connect.NewError(connect.CodeNotFound, errors.New("no such sandbox")),
		connect.NewError(connect.CodeInvalidArgument, errors.New("bad request")),
		billing,
	} {
		t.Run(connect.CodeOf(err).String(), func(t *testing.T) {
			cp := newFakeCP(t)
			cp.agent.run = lineRun(3, 0)
			cp.iceGate = make(chan struct{})
			cp.gateErr = err
			s := cp.handle(t, cp.client(t, TransportAuto))
			_, got := execOnce(t, s)
			if got == nil || connect.CodeOf(got) != connect.CodeOf(err) {
				t.Fatalf("exec error %v, want code %v", got, connect.CodeOf(err))
			}
			if n := len(cp.authRequests()); n != 0 {
				t.Fatalf("%d AuthorizeRelay calls, want 0", n)
			}
			if s.usesRelay() {
				t.Fatal("the handle moved to the relay")
			}
		})
	}
}
