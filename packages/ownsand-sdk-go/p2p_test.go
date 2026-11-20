package ownsand

import (
	"bytes"
	"context"
	"testing"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/packages/ownsand-sdk-go/ownsandtest"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func TestIdleHandleOpensNoSession(t *testing.T) {
	e := newEnv(t, TransportAuto)
	if e.srv.OpenSessionCalls() != 0 || e.sb.Transport() != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_UNSPECIFIED {
		t.Fatal("handle opened a session without an operation")
	}
	if _, _, _, _, err := e.run(ExecOptions{Command: []string{"echo"}}); err != nil {
		t.Fatal(err)
	}
	if e.srv.OpenSessionCalls() != 1 || e.sb.Transport() != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER {
		t.Fatalf("open sessions %d transport %v", e.srv.OpenSessionCalls(), e.sb.Transport())
	}
	if len(e.srv.AuthorizeCalls()) != 0 {
		t.Fatal("peer-to-peer exec asked for a relay grant")
	}
}

func TestFallbackToRelayWhenDataChannelDoesNotOpen(t *testing.T) {
	e := newEnv(t, TransportAuto)
	e.srv.SetPeerUnreachable(true)
	start := e.srv.Clock.Now()
	res, _, out, _, err := e.run(ExecOptions{Command: []string{"echo", "via relay"}})
	if err != nil || out != "via relay\n" || res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY {
		t.Fatalf("%v %q %+v", err, out, res)
	}
	if el := e.srv.Clock.Now().Sub(start); el < peerOpenTimeout {
		t.Fatalf("fell back after %v", el)
	}
	// The handle stays on the relay.
	if _, _, _, _, err := e.run(ExecOptions{Command: []string{"echo"}}); err != nil || e.srv.OpenSessionCalls() != 1 {
		t.Fatalf("%v sessions %d", err, e.srv.OpenSessionCalls())
	}
}

func TestChannelFailureContinuesExecOnRelay(t *testing.T) {
	e := newEnv(t, TransportAuto)
	g := newGate()
	e.program("two", func(ctx context.Context, x *ownsandtest.ExecIO) {
		x.Stdout([]byte("p2p."))
		g.wait(ctx)
		x.Stdout([]byte("relay."))
		x.Exit(0)
	})
	st, err := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"two"}})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := st.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.FailPeerSessions()
	waitFor(t, func() bool { return len(e.srv.RelayCalls()) >= 1 })
	g.release()
	res, evs, out, _, err := collect(st)
	if err != nil || out != "relay." || count(evs, ExecEventStarted) != 0 || res.Transport != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY {
		t.Fatalf("%v %q %v", err, out, kinds(evs))
	}
	calls := e.srv.RelayCalls()
	if calls[0].RPC != ownsandtest.RelayAttachExec || calls[0].Offset != int64(len("p2p.")) || e.srv.Agent.TotalRuns() != 1 {
		t.Fatalf("calls %+v", calls)
	}
}

func TestSessionEndContinuesOnNewSession(t *testing.T) {
	e := newEnv(t, TransportPeerToPeer)
	g := newGate()
	e.program("two", func(ctx context.Context, x *ownsandtest.ExecIO) {
		x.Stdout([]byte("one."))
		g.wait(ctx)
		x.Stdout([]byte("two."))
		x.Exit(0)
	})
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"two"}})
	for range 2 {
		if _, err := st.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	e.srv.EndPeerSessions(ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE)
	waitFor(t, func() bool { return e.srv.OpenSessionCalls() == 2 })
	g.release()
	_, _, out, _, err := collect(st)
	if err != nil || out != "two." {
		t.Fatalf("%v %q", err, out)
	}
	reqs := e.srv.PeerRequests()
	if len(reqs) != 2 || reqs[1] != ownsandv1.DataFrameKind_DATA_FRAME_KIND_EXEC_ATTACH_REQUEST || e.srv.Agent.TotalRuns() != 1 {
		t.Fatalf("requests %v", reqs)
	}
	if len(e.srv.RelayCalls()) != 0 {
		t.Fatal("session end moved the handle to the relay")
	}
}

func TestNewSessionThatCannotBeAuthorizedEndsOperation(t *testing.T) {
	e := newEnv(t, TransportAuto)
	g := newGate()
	defer g.release()
	e.program("wait", func(ctx context.Context, x *ownsandtest.ExecIO) { g.wait(ctx); x.Exit(0) })
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"wait"}})
	if _, err := st.Recv(); err != nil {
		t.Fatal(err)
	}
	e.srv.RevokeCredential(ownsandtest.DefaultAPIKey)
	e.srv.EndPeerSessions(ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
	_, err := st.Recv()
	if CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("err = %v", err)
	}
	if len(e.srv.RelayCalls()) != 0 {
		t.Fatal("fell back to the relay after an authorization error")
	}
}

func TestTruncatedAttachEventOrderOnPeerToPeer(t *testing.T) {
	e := newEnv(t, TransportPeerToPeer)
	e.srv.Agent.KeepBytes = 1000
	g := newGate()
	written := make(chan struct{})
	e.program("flood", func(ctx context.Context, x *ownsandtest.ExecIO) {
		x.Stdout(bytes.Repeat([]byte("a"), 100))
		g.wait(ctx)
		x.Stdout(bytes.Repeat([]byte("b"), 3000))
		close(written)
		x.Exit(0)
	})
	// The new session opens only after the output passed the kept window.
	e.srv.OnOpenSession = func(n int) {
		if n == 2 {
			g.release()
			<-written
		}
	}
	st, _ := e.sb.Exec(e.ctx, ExecOptions{Command: []string{"flood"}})
	for range 2 {
		if _, err := st.Recv(); err != nil {
			t.Fatal(err)
		}
	}
	// End the session before the agent sends the new output.
	e.srv.EndPeerSessions(ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED)
	res, evs, _, _, err := collect(st)
	if err != nil {
		t.Fatal(err)
	}
	checkTruncatedOrder(t, res, evs)
}

func TestReadAndWriteContinueAcrossSessionEnd(t *testing.T) {
	e := newEnv(t, TransportPeerToPeer)
	data := file(3 << 20)
	r := newPausingReader(data, 1<<20)
	done := make(chan error, 1)
	go func() { _, err := e.sb.WriteFile(e.ctx, "/w", r, 0o600); done <- err }()
	<-r.reached
	e.srv.EndPeerSessions(ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE)
	close(r.pause)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	got, err := e.sb.ReadFileBytes(e.ctx, "/w")
	if err != nil || !bytes.Equal(got, data) {
		t.Fatal(err)
	}
	if e.srv.OpenSessionCalls() < 2 {
		t.Fatal("no new session")
	}
}
