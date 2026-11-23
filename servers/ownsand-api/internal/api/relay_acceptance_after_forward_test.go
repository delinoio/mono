// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/rand"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// bodyDelay is how long the caller's request body is held back, so that the
// stream is open and credited at the agent before the writer stalls and the
// request frame is queued behind the stall.
const bodyDelay = 1500 * time.Millisecond

// acceptanceCall is an Exec call whose request frame is queued behind a
// blocked connection writer while the outbound queue still has room and the
// agent has already granted the stream's credit.
type acceptanceCall struct {
	relay   *testenv.FakeRelay
	execID  string
	started time.Time
	// done receives the result of the call and the time it took.
	done chan acceptanceResult
}

type acceptanceResult struct {
	events []*ownsandv1.ExecEvent
	err    error
	took   time.Duration
}

// startStalledExec starts an Exec call whose request body arrives bodyDelay
// after the headers. The control plane allocates the stream at the headers
// and the fake agent grants its credit when it reads the stream open frame.
// The test then stalls the writer of the agent relay connection (the fake
// agent stops reading, and large frames of another stream fill the HTTP/2
// flow control window; the 1024 places of the queue stay free), so the
// request frame that follows the body is queued but not written. serve is the
// fake agent's answer to the request frame. The request deadline is
// requestDeadline, counted from the stream allocation; the acceptance timeout
// is one second.
func startStalledExec(t *testing.T, requestDeadline time.Duration, serve func(s *testenv.RelayStream, f *ownsandv1.DataFrame)) *acceptanceCall {
	t.Helper()
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 10 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: requestDeadline, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, agent, sandboxID := runningSandbox(t, env)
	c := &acceptanceCall{done: make(chan acceptanceResult, 1)}
	c.relay = agent.ConnectRelay(serve)
	c.relay.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	stop := keepAlive(c.relay)
	t.Cleanup(stop)

	req := newExec(org.ID, sandboxID)
	c.execID = req.GetExecId()
	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	client := delayedRelay(env, org, grant.GetGrantId(), bodyDelay)
	c.started = time.Now()
	go func() {
		es, err := client.Exec(context.Background(), connect.NewRequest(req))
		if err != nil {
			c.done <- acceptanceResult{err: err, took: time.Since(c.started)}
			return
		}
		events, err := collectExec(t, es)
		c.done <- acceptanceResult{events: events, err: err, took: time.Since(c.started)}
	}()
	// The stream exists once the agent read its open frame; its credit grant
	// follows at once.
	streamOf(c.relay, grant.GetGrantId())
	time.Sleep(200 * time.Millisecond)

	plane := env.App.Plane
	g := plane.Grants.Get(grant.GetGrantId())
	if g == nil {
		t.Fatal("the grant of the call is gone")
	}
	conn := plane.Relays.Get(g.DeviceID, g.ChannelGen)
	if conn == nil {
		t.Fatal("the agent relay connection is gone")
	}
	filler, err := conn.Open(context.Background(), "filler-grant", g.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	c.relay.WaitFrame("filler stream open", func(f *ownsandv1.DataFrame) bool {
		return f.GetRelayStreamOpen() != nil && f.GetStreamId() == filler.ID
	})
	time.Sleep(200 * time.Millisecond)
	c.relay.PauseReads()
	t.Cleanup(c.relay.ResumeReads)
	// Random data: Connect compresses a repetitive payload to a few bytes,
	// which would never fill the flow control window of the agent's stream.
	data := make([]byte, 512<<10)
	_, _ = rand.Read(data)
	for i := 0; i < 12; i++ {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := filler.Send(sctx, datapath.MustNewFrame(0, "", &ownsandv1.WriteFileRequest{Data: data}))
		cancel()
		if err != nil {
			t.Fatalf("queue a large frame: %v", err)
		}
	}
	// The writer is blocked, but the outbound queue has room: a small frame
	// is accepted at once.
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = filler.Send(sctx, datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}))
	cancel()
	if err != nil {
		t.Fatalf("the outbound queue is full, want room behind the blocked writer: %v", err)
	}
	if left := bodyDelay - time.Since(c.started); left < 300*time.Millisecond {
		t.Fatalf("the writer stalled only %v before the request body arrived, want the stall in place first", left)
	}
	return c
}

// requestReached reports whether the fake agent received the request frame.
func (c *acceptanceCall) requestReached() bool {
	for _, f := range c.relay.Frames() {
		if f.GetExecRequest().GetExecId() == c.execID {
			return true
		}
	}
	return false
}

// holdWriter keeps the writer blocked until d after the request body arrived
// and checks that the request frame stayed unwritten meanwhile; it fails the
// test when the writer was not blocked, because the test would then not cover
// the queued state.
func (c *acceptanceCall) holdWriter(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case r := <-c.done:
		t.Fatalf("the call ended after %v while the writer was blocked: %v", r.took, r.err)
	case <-time.After(time.Until(c.started.Add(bodyDelay + d))):
	}
	if c.requestReached() {
		t.Fatal("the request frame reached the agent although its reads were paused")
	}
}

func (c *acceptanceCall) wait(t *testing.T, within time.Duration) acceptanceResult {
	t.Helper()
	select {
	case r := <-c.done:
		return r
	case <-time.After(within):
		t.Fatalf("the Exec call is still running %v after the writer was released", within)
		return acceptanceResult{}
	}
}

// TestRelayAcceptanceStartsAfterTheRequestWasWritten checks that the
// acceptance timeout counts from the moment the connection writer sent the
// request frame and not from its insertion into the outbound queue: the
// frame waits 1.6 s behind a blocked writer (longer than the 1 s acceptance
// timeout), and the agent's prompt answer after the release is accepted.
func TestRelayAcceptanceStartsAfterTheRequestWasWritten(t *testing.T) {
	c := startStalledExec(t, 20*time.Second, serveExec)
	c.holdWriter(t, 1600*time.Millisecond)
	c.relay.ResumeReads()
	r := c.wait(t, 5*time.Second)
	if r.err != nil || !hasAccepted(r.events) {
		t.Fatalf("exec after a long queue wait: %v, events %v; want the accepted request", r.err, r.events)
	}
}

// TestRelayForwardingDeadlineHoldsBehindBlockedWriter checks that a request
// frame that the writer cannot send before the request deadline ends the call
// with deadline_exceeded (not unavailable from the acceptance timeout, which
// has not started) and that the agent sees the stream close.
func TestRelayForwardingDeadlineHoldsBehindBlockedWriter(t *testing.T) {
	c := startStalledExec(t, bodyDelay+1500*time.Millisecond, serveExec)
	r := c.wait(t, 5*time.Second)
	if testenv.Code(r.err) != connect.CodeDeadlineExceeded {
		t.Fatalf("exec behind a blocked writer: %v after %v, want deadline_exceeded", r.err, r.took)
	}
	if r.took < bodyDelay+1400*time.Millisecond {
		t.Fatalf("the call ended after %v, before the request deadline", r.took)
	}
	c.relay.ResumeReads()
	c.relay.WaitFrame("stream close of the timed out call", func(f *ownsandv1.DataFrame) bool {
		return f.GetRelayStreamClose() != nil && f.GetStreamId() != 0 && f.GetStreamId() != c.fillerStream()
	})
}

// fillerStream returns the stream_id of the filler stream, the first stream
// that the test opened on the connection.
func (c *acceptanceCall) fillerStream() uint64 {
	for _, f := range c.relay.Frames() {
		if f.GetRelayStreamOpen().GetGrantId() == "filler-grant" {
			return f.GetStreamId()
		}
	}
	return 0
}

// TestRelayAcceptanceTimeoutAfterWrittenRequest checks that a request that
// the writer sent but that the agent never accepts still ends with
// unavailable one acceptance timeout after the write, not after the call
// started.
func TestRelayAcceptanceTimeoutAfterWrittenRequest(t *testing.T) {
	c := startStalledExec(t, 20*time.Second, func(*testenv.RelayStream, *ownsandv1.DataFrame) {})
	c.holdWriter(t, 600*time.Millisecond)
	c.relay.ResumeReads()
	r := c.wait(t, 5*time.Second)
	if testenv.Code(r.err) != connect.CodeUnavailable {
		t.Fatalf("exec without acceptance: %v after %v, want unavailable", r.err, r.took)
	}
	if r.took < bodyDelay+1400*time.Millisecond {
		t.Fatalf("the acceptance timeout ended the call after %v, want the body delay plus about 0.6 s of queue wait plus 1 s", r.took)
	}
}
