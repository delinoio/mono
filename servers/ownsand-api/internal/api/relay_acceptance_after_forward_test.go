// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

const (
	relayAcceptanceWindow = time.Second
	stalledExecPhaseLimit = 5 * time.Second
)

// gatedExecTransport lets the HTTP transport send the headers while the
// fixture controls when it can read the body.
type gatedExecTransport struct {
	base     http.RoundTripper
	bodyGate <-chan struct{}
}

func (tr gatedExecTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Body = &gatedExecBody{
		ReadCloser: r.Body,
		ctx:        r.Context(),
		bodyGate:   tr.bodyGate,
		closed:     make(chan struct{}),
	}
	return tr.base.RoundTrip(r)
}

type gatedExecBody struct {
	io.ReadCloser
	ctx      context.Context
	bodyGate <-chan struct{}
	closed   chan struct{}
	close    sync.Once
}

func (b *gatedExecBody) Read(p []byte) (int, error) {
	select {
	case <-b.bodyGate:
		return b.ReadCloser.Read(p)
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	}
}

func (b *gatedExecBody) Close() error {
	b.close.Do(func() { close(b.closed) })
	return b.ReadCloser.Close()
}

type acceptanceCall struct {
	relay         *testenv.FakeRelay
	execID        string
	streamID      uint64
	releaseBody   func()
	releaseWriter func()
	enqueued      <-chan time.Time
	sent          <-chan time.Time
	done          chan struct{}
	result        acceptanceResult // Published by closing done.
}

type acceptanceResult struct {
	events      []*ownsandv1.ExecEvent
	err         error
	completedAt time.Time
}

// startStalledExec waits for the agent to see the Exec stream open, then stalls
// the connection writer in a separate stream's metering callback. It returns
// with the body and writer gates closed to traffic.
func startStalledExec(t *testing.T, requestDeadline time.Duration, serve func(s *testenv.RelayStream, f *ownsandv1.DataFrame)) *acceptanceCall {
	t.Helper()
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 10 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: relayAcceptanceWindow,
			RequestDeadline: requestDeadline, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, agent, sandboxID := runningSandbox(t, env)
	req := newExec(org.ID, sandboxID)
	c := &acceptanceCall{execID: req.GetExecId(), done: make(chan struct{})}
	c.relay = agent.ConnectRelay(serve)
	c.relay.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	t.Cleanup(keepAlive(c.relay))

	grant, err := authorizeExec(t, org.OwnerClient, req)
	if err != nil {
		t.Fatal(err)
	}
	plane := env.App.Plane
	g := plane.Grants.Get(grant.GetGrantId())
	if g == nil {
		t.Fatal("the grant of the call is gone")
	}
	conn := plane.Relays.Get(g.DeviceID, g.ChannelGen)
	if conn == nil {
		t.Fatal("the agent relay connection is gone")
	}
	setupCtx, stopSetup := context.WithTimeout(t.Context(), stalledExecPhaseLimit)
	defer stopSetup()
	filler, err := conn.Open(setupCtx, "filler-grant", g.SandboxID)
	if err != nil {
		t.Fatal(err)
	}

	bodyGate, writerGate := make(chan struct{}), make(chan struct{})
	c.releaseBody = sync.OnceFunc(func() { close(bodyGate) })
	c.releaseWriter = sync.OnceFunc(func() { close(writerGate) })
	callCtx, cancelCall := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancelCall()
		c.releaseBody()
		c.releaseWriter()
		select {
		case <-c.done:
		case <-time.After(stalledExecPhaseLimit):
			t.Error("the Exec RPC did not stop during fixture cleanup")
		}
	})

	httpClient := &http.Client{Transport: gatedExecTransport{base: env.Server.Client().Transport, bodyGate: bodyGate}}
	client := ownsandv1connect.NewRelayServiceClient(httpClient, env.Server.URL)
	rpcReq := connect.NewRequest(req)
	rpcReq.Header().Set("Authorization", "Bearer "+testenv.UserToken(org.Owner))
	rpcReq.Header().Set(api.RelayGrantHeader, grant.GetGrantId())
	go func() {
		defer close(c.done)
		stream, err := client.Exec(callCtx, rpcReq)
		var events []*ownsandv1.ExecEvent
		if err == nil {
			events, err = collectExec(t, stream)
		}
		c.result = acceptanceResult{events: events, err: err, completedAt: time.Now()}
	}()

	c.streamID = streamOf(c.relay, grant.GetGrantId())
	c.enqueued, c.sent = conn.WatchExecSendForTest(c.streamID, c.execID)

	writerEntered := make(chan struct{})
	err = filler.SendMetered(setupCtx, datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}), func(int64) {
		close(writerEntered)
		<-writerGate
	})
	if err != nil {
		t.Fatalf("queue the filler frame: %v", err)
	}
	select {
	case <-writerEntered:
	case <-c.done:
		t.Fatalf("the Exec RPC completed before the writer entered its gate: %v", c.result.err)
	case <-time.After(stalledExecPhaseLimit):
		t.Fatal("timed out waiting for the writer to enter its gate")
	}
	return c
}

func (c *acceptanceCall) waitPhase(phase string, reached <-chan time.Time) (time.Time, error) {
	select {
	case at := <-reached:
		return at, nil
	case <-c.done:
		// The observation may already be available when the RPC completes.
		select {
		case at := <-reached:
			if !at.After(c.result.completedAt) {
				return at, nil
			}
		default:
		}
		if c.result.err == nil {
			return time.Time{}, fmt.Errorf("the Exec RPC completed successfully before %s", phase)
		}
		return time.Time{}, fmt.Errorf("the Exec RPC completed before %s: %w", phase, c.result.err)
	case <-time.After(stalledExecPhaseLimit):
		return time.Time{}, fmt.Errorf("timed out waiting for Exec %s", phase)
	}
}

func (c *acceptanceCall) releaseBodyAndWaitForEnqueue(t *testing.T) time.Time {
	t.Helper()
	c.releaseBody()
	queuedAt, err := c.waitPhase("outbound enqueue", c.enqueued)
	if err != nil {
		t.Fatal(err)
	}
	return queuedAt
}

func (c *acceptanceCall) holdWriter(t *testing.T, queuedAt time.Time, duration time.Duration) {
	t.Helper()
	select {
	case <-c.done:
		t.Fatalf("the Exec RPC completed while the writer was gated: %v", c.result.err)
	case <-c.sent:
		t.Fatal("the writer sent the Exec frame while it was gated")
	case <-time.After(time.Until(queuedAt.Add(duration))):
	}
}

func (c *acceptanceCall) waitSent(t *testing.T) time.Time {
	t.Helper()
	sentAt, err := c.waitPhase("transport send completion", c.sent)
	if err != nil {
		t.Fatal(err)
	}
	return sentAt
}

func (c *acceptanceCall) waitResult(t *testing.T) acceptanceResult {
	t.Helper()
	select {
	case <-c.done:
		return c.result
	case <-time.After(stalledExecPhaseLimit):
		t.Fatal("timed out waiting for the Exec RPC result")
		return acceptanceResult{}
	}
}

func (c *acceptanceCall) matchesRequest(f *ownsandv1.DataFrame) bool {
	return f.GetStreamId() == c.streamID && f.GetExecRequest().GetExecId() == c.execID
}

func (c *acceptanceCall) waitStreamClose() {
	c.relay.WaitFrame("close of the Exec stream", func(f *ownsandv1.DataFrame) bool {
		return f.GetStreamId() == c.streamID && f.GetRelayStreamClose() != nil
	})
}

func requireStalledExecError(t *testing.T, err error, code connect.Code, message string) {
	t.Helper()
	var rpcErr *connect.Error
	if !errors.As(err, &rpcErr) || rpcErr.Code() != code || rpcErr.Message() != message {
		t.Fatalf("Exec returned %v, want %s with message %q", err, code, message)
	}
}

// TestRelayAcceptanceStartsAfterTheRequestWasWritten holds the frame in
// the outbound queue longer than the acceptance window and then lets the
// agent accept and complete the request.
func TestRelayAcceptanceStartsAfterTheRequestWasWritten(t *testing.T) {
	t.Parallel()
	c := startStalledExec(t, 20*time.Second, serveExec)
	queuedAt := c.releaseBodyAndWaitForEnqueue(t)
	const queueHold = 1600 * time.Millisecond
	c.holdWriter(t, queuedAt, queueHold)
	c.releaseWriter()
	if held := c.waitSent(t).Sub(queuedAt); held < queueHold {
		t.Fatalf("the writer sent after %v in the queue, want at least %v", held, queueHold)
	}
	r := c.waitResult(t)
	if r.err != nil || len(r.events) == 0 || !hasAccepted(r.events) || r.events[len(r.events)-1].GetExited() == nil {
		t.Fatalf("Exec after a long queue wait: %v, events %v; want acceptance and completion", r.err, r.events)
	}
}

// TestRelayForwardingDeadlineHoldsBehindBlockedWriter keeps the writer gated
// through the forwarding deadline and checks the close of the same stream.
func TestRelayForwardingDeadlineHoldsBehindBlockedWriter(t *testing.T) {
	t.Parallel()
	c := startStalledExec(t, 3*time.Second, serveExec)
	c.releaseBodyAndWaitForEnqueue(t)
	r := c.waitResult(t)
	requireStalledExecError(t, r.err, connect.CodeDeadlineExceeded, "the request was not forwarded within the request deadline")
	select {
	case <-c.sent:
		t.Fatal("the writer sent the Exec frame before the gate was released")
	default:
	}
	c.releaseWriter()
	c.waitStreamClose()
}

// An upload deadline has the same status and stream close as a forwarding
// deadline. The fixture must report that the call never reached enqueue.
func TestRelayForwardingFixtureRejectsIncompleteUpload(t *testing.T) {
	t.Parallel()
	c := startStalledExec(t, 3*time.Second, serveExec)
	_, err := c.waitPhase("outbound enqueue", c.enqueued)
	if err == nil || !strings.HasPrefix(err.Error(), "the Exec RPC completed before outbound enqueue: ") {
		t.Fatalf("phase check with the body withheld returned %v, want RPC completion before outbound enqueue", err)
	}
	if testenv.Code(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("upload with the body withheld returned %v, want deadline_exceeded", err)
	}
	c.releaseWriter()
	c.waitStreamClose()
	for _, f := range c.relay.Frames() {
		if c.matchesRequest(f) {
			t.Fatalf("the agent received the Exec request of stream %d while the upload was withheld", c.streamID)
		}
	}
}

// TestRelayAcceptanceTimeoutAfterWrittenRequest keeps the agent silent after
// it receives the request and measures the timeout from transport completion.
func TestRelayAcceptanceTimeoutAfterWrittenRequest(t *testing.T) {
	t.Parallel()
	c := startStalledExec(t, 20*time.Second, nil)
	queuedAt := c.releaseBodyAndWaitForEnqueue(t)
	c.holdWriter(t, queuedAt, 600*time.Millisecond)
	c.releaseWriter()
	sentAt := c.waitSent(t)
	c.relay.WaitFrame("Exec request", c.matchesRequest)
	r := c.waitResult(t)
	requireStalledExecError(t, r.err, connect.CodeUnavailable, "the agent did not accept the request")
	if elapsed := r.completedAt.Sub(sentAt); elapsed < relayAcceptanceWindow-200*time.Millisecond || elapsed > relayAcceptanceWindow+750*time.Millisecond {
		t.Fatalf("the acceptance timeout ended the call %v after transport send completion, want about %v", elapsed, relayAcceptanceWindow)
	}
}
