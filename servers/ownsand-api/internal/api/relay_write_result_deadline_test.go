// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/ownsand/relaybinding"
	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// writeResultCall is one WriteFile call against a fake agent that answers
// only when the test releases the result.
type writeResultCall struct {
	env       *testenv.Env
	relay     *testenv.FakeRelay
	grant     string
	stream    uint64
	sandboxID string
	// release makes the fake agent send the write result.
	release chan struct{}
	// done receives the result of the call.
	done   chan error
	cancel context.CancelFunc
	// startedAt is when the call was sent; deadline is its request deadline.
	startedAt time.Time
	deadline  time.Duration
	resp      chan *ownsandv1.WriteFileResponse
}

// startWriteResultCall starts a WriteFile call with the given request
// deadline and waits until the agent holds the write chunk.
func startWriteResultCall(t *testing.T, deadline time.Duration) *writeResultCall {
	t.Helper()
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming = &api.RelayTiming{Rotation: 3 * time.Second, Keepalive: 200 * time.Millisecond, Acceptance: time.Second,
			RequestDeadline: deadline, GrantAck: time.Second, Signaling: 2 * time.Second}
	})
	org, agent, sandboxID := runningSandbox(t, env)
	c := &writeResultCall{deadline: deadline, sandboxID: sandboxID, env: env, release: make(chan struct{}), done: make(chan error, 1), resp: make(chan *ownsandv1.WriteFileResponse, 1)}
	c.relay = agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
		w := f.GetFileWriteChunk()
		if w == nil {
			return
		}
		<-c.release
		s.Send(&ownsandv1.WriteFileResponse{WriteId: w.GetWriteId(), Offset: w.GetOffset(), Length: int64(len(w.GetData()))})
	})
	c.relay.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })

	writeID, _ := uuidv7.NewString()
	salt, _ := relaybinding.NewSalt()
	write := &ownsandv1.WriteFileRequest{OrganizationId: org.ID, SandboxId: sandboxID, WriteId: writeID, Path: "/tmp/x", BindingSalt: salt, Data: []byte("chunk")}
	c.grant = authorize(t, org.OwnerClient, org.ID, sandboxID, ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_WRITE_FILE, writeID, relaybinding.WriteFileDigest(write))
	ctx, cancel := context.WithCancel(context.Background())
	c.cancel = cancel
	t.Cleanup(cancel)
	client := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(c.grant)).Relay
	c.startedAt = time.Now()
	go func() {
		resp, err := client.WriteFile(ctx, connect.NewRequest(write))
		if err == nil {
			c.resp <- resp.Msg
		}
		c.done <- err
	}()
	c.stream = streamOf(c.relay, c.grant)
	c.relay.WaitFrame("write chunk", func(f *ownsandv1.DataFrame) bool { return f.GetStreamId() == c.stream && f.GetFileWriteChunk() != nil })
	return c
}

// fillOutboundQueue stalls the agent relay connection and fills its outbound
// queue: the fake agent stops reading, so the writer blocks once the HTTP/2
// flow control window of the connection stream is full, and then the 1024
// ordinary places of the queue fill up behind it. It uses extra streams of
// the same connection, so the write call under test does not take part.
func fillOutboundQueue(t *testing.T, c *writeResultCall) {
	t.Helper()
	plane := c.env.App.Plane
	g := plane.Grants.Get(c.grant)
	if g == nil {
		t.Fatal("the grant of the call is gone")
	}
	conn := plane.Relays.Get(g.DeviceID, g.ChannelGen)
	if conn == nil {
		t.Fatal("the agent relay connection is gone")
	}
	ctx := context.Background()
	filler, err := conn.Open(ctx, "filler-grant", g.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	// The agent grants the credit of the filler stream when it reads the
	// stream open frame, so wait for that before it stops reading.
	c.relay.WaitFrame("filler stream open", func(f *ownsandv1.DataFrame) bool {
		return f.GetRelayStreamOpen() != nil && f.GetStreamId() == filler.ID
	})
	time.Sleep(200 * time.Millisecond)
	c.relay.PauseReads()
	t.Cleanup(c.relay.ResumeReads)

	// Random data: Connect compresses a repetitive payload to a few bytes,
	// which would never fill the window of the agent's stream.
	data := make([]byte, 512<<10)
	_, _ = rand.Read(data)
	big := func() *ownsandv1.DataFrame {
		return datapath.MustNewFrame(0, "", &ownsandv1.WriteFileRequest{Data: data})
	}
	for i := 0; i < 12; i++ {
		sctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := filler.Send(sctx, big())
		cancel()
		if err != nil {
			t.Fatalf("queue a large frame: %v", err)
		}
	}
	// Small frames then fill the ordinary places, until the queue refuses
	// them. A send that merely ran late on a busy machine must not pass for a
	// full queue, so three refusals in a row, each after a wait, count.
	refused := 0
	for i := 0; refused < 3; i++ {
		if i > 4096+3 {
			t.Fatal("the outbound queue of the agent connection never filled")
		}
		sctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
		err := filler.Send(sctx, datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}))
		cancel()
		switch {
		case err == nil:
			refused = 0
		case errors.Is(err, context.DeadlineExceeded):
			refused++
		default:
			t.Fatalf("fill the outbound queue: %v", err)
		}
	}
}

// sendWriteResult releases the agent's write result and waits until the
// control plane has it, which shows as the call's result frame reaching the
// stream queue: the call then waits in the credit return.
func (c *writeResultCall) sendWriteResult() { close(c.release) }

func (c *writeResultCall) waitDone(t *testing.T, within time.Duration) error {
	t.Helper()
	select {
	case err := <-c.done:
		return err
	case <-time.After(within):
		t.Fatalf("the WriteFile call is still running %v after the agent answered", within)
		return nil
	}
}

// resultCreditReturns counts the window updates of the call's stream that
// return exactly the credit of the agent's write result.
func (c *writeResultCall) resultCreditReturns(result *ownsandv1.WriteFileResponse) int {
	want := datapath.FrameCost(datapath.MustNewFrame(c.stream, c.sandboxID, result))
	n := 0
	for _, f := range c.relay.Frames() {
		if f.GetStreamId() == c.stream && f.GetRelayWindowUpdate().GetIncrementBytes() == want {
			n++
		}
	}
	return n
}

// callFrames returns the frames counter of the relay_rpc record of the call
// and whether the record exists.
func (c *writeResultCall) callFrames() (int, bool) {
	for _, line := range strings.Split(c.env.Logs.String(), "\n") {
		var rec struct {
			Msg     string `json:"msg"`
			GrantID string `json:"grant_id"`
			Frames  int    `json:"frames"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "relay_rpc" && rec.GrantID == c.grant {
			return rec.Frames, true
		}
	}
	return 0, false
}

// TestRelayWriteFileResultCreditReturnHonorsDeadline checks that a credit
// return that waits for room in a full outbound queue cannot hold a WriteFile
// call beyond its request deadline: the call fails with deadline_exceeded at
// the deadline and its stream close frame is queued behind the frames that
// are already there.
func TestRelayWriteFileResultCreditReturnHonorsDeadline(t *testing.T) {
	// The deadline counts from the stream allocation, and stalling the
	// connection takes a while, so it leaves room for that.
	c := startWriteResultCall(t, 4*time.Second)
	fillOutboundQueue(t, c)
	remaining := time.Until(c.startedAt.Add(c.deadline))
	if remaining < 500*time.Millisecond {
		t.Fatalf("stalling the connection took %v of the %v deadline; too slow to test the credit return", c.deadline-remaining, c.deadline)
	}
	c.sendWriteResult()
	// The agent answered before the deadline. A credit return that waits
	// without the call deadline never ends on its own.
	err := c.waitDone(t, remaining+2*time.Second)
	took := time.Since(c.startedAt)
	if testenv.Code(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("WriteFile = %v after %v, want deadline_exceeded", err, took)
	}
	if took < c.deadline-100*time.Millisecond {
		t.Fatalf("WriteFile ended after %v, before its %v deadline", took, c.deadline)
	}
	waitRelayRPC(t, c.env, c.grant, connect.CodeDeadlineExceeded)

	// The reserved final place holds the stream close frame although the
	// queue is full; it reaches the agent once the agent reads again.
	c.relay.ResumeReads()
	c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
		return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
	})
}

// TestRelayWriteFileCallerCancelDuringCreditReturn checks that a caller that
// cancels while the credit return waits keeps its precedence over the
// deadline: the handler ends with the caller's cancellation.
func TestRelayWriteFileCallerCancelDuringCreditReturn(t *testing.T) {
	c := startWriteResultCall(t, 30*time.Second)
	fillOutboundQueue(t, c)
	c.sendWriteResult()
	time.Sleep(200 * time.Millisecond)
	c.cancel()
	err := c.waitDone(t, 5*time.Second)
	if testenv.Code(err) != connect.CodeCanceled {
		t.Fatalf("WriteFile = %v, want canceled", err)
	}
	waitRelayRPC(t, c.env, c.grant, connect.CodeCanceled)
	c.relay.ResumeReads()
	c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
		return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
	})
}

// TestRelayWriteFileResultWithinBudget checks that a result that arrives in
// time returns the agent's response, accounts for the result once and returns
// the credit once.
func TestRelayWriteFileResultWithinBudget(t *testing.T) {
	c := startWriteResultCall(t, 2*time.Second)
	c.sendWriteResult()
	if err := c.waitDone(t, 5*time.Second); err != nil {
		t.Fatalf("WriteFile = %v, want success", err)
	}
	resp := <-c.resp
	if resp.GetLength() != int64(len("chunk")) || resp.GetOffset() != 0 {
		t.Fatalf("response = %v, want the agent's result for the 5 byte chunk", resp)
	}
	testenv.Eventually(t, "relay_rpc record of the call", func() bool { _, ok := c.callFrames(); return ok })
	if n, _ := c.callFrames(); n != 1 {
		t.Fatalf("relay_rpc frames = %d, want 1 (the result counted once)", n)
	}
	testenv.Eventually(t, "credit return of the result", func() bool { return c.resultCreditReturns(resp) >= 1 })
	if n := c.resultCreditReturns(resp); n != 1 {
		t.Fatalf("credit returns of the result on stream %d = %d, want exactly one", c.stream, n)
	}
}
