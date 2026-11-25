// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestRelayWriteFileTerminalBeforeCredit(t *testing.T) {
	for _, terminal := range writeTerminalResponses() {
		t.Run(terminal.name, func(t *testing.T) {
			c := startWriteCall(t, 15*time.Second, 0)
			defer c.sendWriteResult()
			grant := c.env.App.Plane.Grants.Get(c.grant)
			if grant == nil {
				t.Fatal("the write grant was lost")
			}
			conn := c.env.App.Plane.Relays.Get(grant.DeviceID, grant.ChannelGen)
			if conn == nil {
				t.Fatal("the fake agent's relay connection was lost")
			}
			c.relay.SendFrame(datapath.MustNewFrame(c.stream, c.sandboxID, terminal.payload))
			err := c.waitDone(t, 3*time.Second)
			want, isError := terminal.payload.(*ownsandv1.ErrorDetail)
			if !isError {
				want = &ownsandv1.ErrorDetail{
					Code:             ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE,
					Message:          "the agent ended the relay stream",
					SessionEndReason: terminal.payload.(*ownsandv1.SessionEnd).GetReason(),
				}
			}
			code := connect.Code(want.GetCode())
			if testenv.Code(err) != code || !proto.Equal(errDetail(err), want) {
				t.Fatalf("WriteFile = %v, detail %v, want %s with %v", err, errDetail(err), code, want)
			}
			waitRelayRPC(t, c.env, c.grant, code)
			if got := conn.StreamCount(); got != 0 {
				t.Fatalf("open streams after agent ended the call = %d, want 0", got)
			}

			// The echo follows the late credit through the relay reader and
			// writer. Inspect all frames up to that point for a write or close.
			c.relay.SendFrame(datapath.MustNewFrame(c.stream, c.sandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: 8 << 20}))
			c.relay.SendFrame(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{EchoRequested: true}))
			c.relay.WaitFrame("keepalive echo", func(f *ownsandv1.DataFrame) bool {
				return f.GetStreamId() == 0 && f.GetRelayKeepalive() != nil && !f.GetRelayKeepalive().GetEchoRequested()
			})
			for _, f := range c.relay.Frames() {
				if f.GetStreamId() == c.stream && (f.GetFileWriteChunk() != nil || f.GetRelayStreamClose() != nil) {
					t.Fatalf("agent saw %s for a write it rejected before credit", f.GetKind())
				}
			}
		})
	}
}

func TestRelayWriteFileNoCreditLocalExit(t *testing.T) {
	for _, code := range []connect.Code{connect.CodeDeadlineExceeded, connect.CodeCanceled} {
		t.Run(code.String(), func(t *testing.T) {
			c := startWriteCall(t, 2*time.Second, 0)
			defer c.sendWriteResult()
			if code == connect.CodeCanceled {
				c.cancel()
			}
			if err := c.waitDone(t, 5*time.Second); testenv.Code(err) != code {
				t.Fatalf("WriteFile = %v, want %s", err, code)
			}
			waitRelayRPC(t, c.env, c.grant, code)
			c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
				return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
			})
			for _, f := range c.relay.Frames() {
				if f.GetStreamId() == c.stream && f.GetFileWriteChunk() != nil {
					t.Fatal("the relay forwarded a write without credit")
				}
			}
		})
	}
}
