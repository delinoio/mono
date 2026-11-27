// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

// holdWriteResponse pauses the handler after it dequeues an agent response
// until the call context ends. The channel signals that the handler paused.
func holdWriteResponse(t *testing.T, c *writeResultCall) <-chan struct{} {
	t.Helper()
	reached := make(chan struct{})
	plane := c.env.App.Plane
	plane.SetAfterWriteResponseDequeuedForTest(func(cctx context.Context) {
		close(reached)
		<-cctx.Done()
	})
	t.Cleanup(func() { plane.SetAfterWriteResponseDequeuedForTest(nil) })
	return reached
}

func (c *writeResultCall) requireNoResultCreditReturn(t *testing.T, waiting <-chan struct{}) {
	t.Helper()
	select {
	case <-waiting:
		t.Fatalf("stream %d: credit return for the late result reached the full outbound queue", c.stream)
	default:
	}
	c.requireCallFrames(t, 0)
	for _, f := range c.relay.Frames() {
		if f.GetStreamId() == c.stream && f.GetRelayWindowUpdate() != nil {
			t.Fatalf("stream %d: received a window update for the late result", c.stream)
		}
	}
}

// TestRelayWriteFileLateResultIsRejected checks that a write result that is
// already queued when the call budget expires does not end the call with
// success: the call fails with deadline_exceeded, the stream close frame
// reaches the agent, and the expired result is neither counted in the call
// record nor consumed (no window update returns its credit).
func TestRelayWriteFileLateResultIsRejected(t *testing.T) {
	c := startWriteResultCall(t, 4*time.Second)
	waiting := fillOutboundQueue(t, c).WatchWindowUpdateWaitForTest(c.stream)
	reached := holdWriteResponse(t, c)
	c.releaseWriteResult()
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
	c.relay.ResumeReads()
	c.waitStreamClose(t)
	c.requireNoResultCreditReturn(t, waiting)
}

// TestRelayWriteFileLateResultCallerCancelWins checks that a caller that left
// before the handler looked at the queued result keeps its precedence over the
// expired call deadline.
func TestRelayWriteFileLateResultCallerCancelWins(t *testing.T) {
	c := startWriteResultCall(t, 30*time.Second)
	waiting := fillOutboundQueue(t, c).WatchWindowUpdateWaitForTest(c.stream)
	reached := holdWriteResponse(t, c)
	c.releaseWriteResult()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the handler never took the write result off the queue")
	}
	// Cancel while the handler holds the result before accounting for it.
	c.cancel()
	if err := c.waitDone(t, 10*time.Second); testenv.Code(err) != connect.CodeCanceled {
		t.Fatalf("WriteFile = %v, want canceled", err)
	}
	waitRelayRPC(t, c.env, c.grant, connect.CodeCanceled)
	c.relay.ResumeReads()
	c.waitStreamClose(t)
	c.requireNoResultCreditReturn(t, waiting)
}

// TestRelayWriteFileLateTerminalResponse checks that local cancellation and
// expiry take precedence over both types of queued terminal agent frame.
func TestRelayWriteFileLateTerminalResponse(t *testing.T) {
	for _, terminal := range writeTerminalResponses() {
		for _, code := range []connect.Code{connect.CodeDeadlineExceeded, connect.CodeCanceled} {
			t.Run(terminal.name+"/"+code.String(), func(t *testing.T) {
				c := startWriteResultCall(t, 3*time.Second)
				defer c.releaseWriteResult()
				reached := holdWriteResponse(t, c)
				c.relay.SendFrame(datapath.MustNewFrame(c.stream, c.sandboxID, terminal.payload))
				select {
				case <-reached:
				case <-time.After(5 * time.Second):
					t.Fatal("the handler never dequeued the terminal frame")
				}
				if code == connect.CodeCanceled {
					c.cancel()
				}
				err := c.waitDone(t, 5*time.Second)
				if testenv.Code(err) != code || errDetail(err) != nil {
					t.Fatalf("WriteFile = %v, detail %v, want local %s", err, errDetail(err), code)
				}
				waitRelayRPC(t, c.env, c.grant, code)
				c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
					return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
				})
			})
		}
	}
}

type writeTerminalResponse struct {
	name    string
	payload proto.Message
}

func writeTerminalResponses() []writeTerminalResponse {
	return []writeTerminalResponse{
		{"error", &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, Message: "the write was rejected"}},
		{"session_end", &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID}},
	}
}
