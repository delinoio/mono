// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// holdWriteResult makes the WriteFile handler wait, after it took the agent's
// write result off the stream queue, until the call deadline has expired. It
// returns a channel that closes when the handler reached that point. The hook
// waits on the context of the call deadline, not on the clock, so the call is
// expired for certain when the handler goes on.
func holdWriteResult(t *testing.T, c *writeResultCall) <-chan struct{} {
	t.Helper()
	reached := make(chan struct{})
	plane := c.env.App.Plane
	plane.SetAfterWriteResultDequeuedForTest(func(cctx context.Context) {
		close(reached)
		<-cctx.Done()
	})
	t.Cleanup(func() { plane.SetAfterWriteResultDequeuedForTest(nil) })
	return reached
}

// TestRelayWriteFileLateResultIsRejected checks that a write result that is
// already queued when the call budget expires does not end the call with
// success: the call fails with deadline_exceeded, the stream close frame
// reaches the agent, and the expired result is neither counted in the call
// record nor consumed (no window update returns its credit).
func TestRelayWriteFileLateResultIsRejected(t *testing.T) {
	c := startWriteResultCall(t, 3*time.Second)
	reached := holdWriteResult(t, c)
	c.sendWriteResult()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never took the write result off the queue")
	}
	err := c.waitDone(t, 10*time.Second)
	if testenv.Code(err) != connect.CodeDeadlineExceeded {
		t.Fatalf("WriteFile = %v, want deadline_exceeded", err)
	}
	waitRelayRPC(t, c.env, c.grant, connect.CodeDeadlineExceeded)
	// Frames of one stream leave in order, so once the close frame is there
	// every window update the handler queued before it is there too.
	c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
		return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
	})
	if n, ok := c.callFrames(); !ok || n != 0 {
		t.Fatalf("relay_rpc frames = %d (record found: %v), want 0: an expired result is not counted", n, ok)
	}
	result := &ownsandv1.WriteFileResponse{Length: int64(len("chunk"))}
	if n := c.resultCreditReturns(result); n != 0 {
		t.Fatalf("credit returns of the expired result = %d, want none", n)
	}
}

// TestRelayWriteFileLateResultCallerCancelWins checks that a caller that left
// before the handler looked at the queued result keeps its precedence over the
// expired call deadline.
func TestRelayWriteFileLateResultCallerCancelWins(t *testing.T) {
	c := startWriteResultCall(t, 3*time.Second)
	reached := holdWriteResult(t, c)
	c.sendWriteResult()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never took the write result off the queue")
	}
	// The caller leaves while the handler waits; the deadline expires after
	// that, so both contexts are done when the handler goes on.
	c.cancel()
	if err := c.waitDone(t, 10*time.Second); testenv.Code(err) != connect.CodeCanceled {
		t.Fatalf("WriteFile = %v, want canceled", err)
	}
	waitRelayRPC(t, c.env, c.grant, connect.CodeCanceled)
	c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
		return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
	})
}
