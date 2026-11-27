// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

func TestRelayExecQueueLimitEndsLikeRotation(t *testing.T) {
	for _, tc := range []struct {
		name     string
		accepted bool
	}{
		{name: "accepted", accepted: true},
		{name: "before acceptance"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := relay.DefaultLimits()
			limits.MaxQueuedFrames = 3
			env := testenv.New(t, func(o *app.Options) {
				o.RelayLimits = &limits
				o.RelayTiming.Acceptance = 30 * time.Second
				o.RelayTiming.RequestDeadline = 10 * time.Second
				o.RelayTiming.Rotation = 30 * time.Second
				if tc.accepted {
					o.RelayTiming.Keepalive = 30 * time.Second
				} else {
					o.RelayTiming.Keepalive = 20 * time.Millisecond
				}
			})
			org, agent, sandboxID := runningSandbox(t, env)
			req := newExec(org.ID, sandboxID)
			forwarded := make(chan *testenv.RelayStream, 1)
			peer := agent.ConnectRelay(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
				if f.GetExecRequest().GetExecId() == req.GetExecId() {
					if tc.accepted {
						s.Send(&ownsandv1.RequestAccepted{})
					}
					forwarded <- s
					return
				}
				s.Send(&ownsandv1.RequestAccepted{})
				s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{
					Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0},
				}}})
			})

			entered, resume := make(chan struct{}), make(chan struct{})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				env.App.Handler().ServeHTTP(&stalledCallerWriter{
					ResponseWriter: w, entered: entered, resume: resume, ctx: r.Context(),
				}, r)
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()

			grant, err := authorizeExec(t, org.OwnerClient, req)
			if err != nil {
				t.Fatal(err)
			}
			client := ownsandv1connect.NewRelayServiceClient(srv.Client(), srv.URL)
			result := make(chan acceptanceResult, 1)
			go func() {
				request := connect.NewRequest(req)
				request.Header().Set("Authorization", "Bearer "+testenv.UserToken(org.Owner))
				request.Header().Set(api.RelayGrantHeader, grant.GetGrantId())
				stream, err := client.Exec(ctx, request)
				if err != nil {
					result <- acceptanceResult{err: err}
					return
				}
				events, err := collectExec(t, stream)
				result <- acceptanceResult{events: events, err: err}
			}()
			var slow *testenv.RelayStream
			select {
			case slow = <-forwarded:
			case <-ctx.Done():
				t.Fatalf("waiting for the forwarded Exec: %v", ctx.Err())
			}
			select {
			case <-entered:
				// The first caller write holds the accepted event or a keepalive.
			case <-ctx.Done():
				t.Fatalf("waiting for the caller write: %v", ctx.Err())
			}
			for i := range limits.MaxQueuedFrames + 1 {
				slow.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{
					Offset: int64(i), Data: []byte("x"),
				}}})
			}
			isSlowClose := func(f *ownsandv1.DataFrame) bool {
				return f.GetStreamId() == slow.ID && f.GetRelayStreamClose() != nil
			}
			peer.WaitFrame("close of the full stream", isSlowClose)
			slow.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Keepalive{Keepalive: &ownsandv1.Keepalive{}}})

			healthy := newExec(org.ID, sandboxID)
			healthyGrant, err := authorizeExec(t, org.OwnerClient, healthy)
			if err != nil {
				t.Fatal(err)
			}
			healthyClient := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(healthyGrant.GetGrantId())).Relay
			healthyStream, err := healthyClient.Exec(ctx, connect.NewRequest(healthy))
			if err != nil {
				t.Fatal(err)
			}
			events, err := collectExec(t, healthyStream)
			if err != nil || !hasAccepted(events) || events[len(events)-1].GetExited() == nil {
				t.Fatalf("healthy Exec events = %v, error = %v", events, err)
			}

			close(resume)
			got := <-result
			if hasAccepted(got.events) != tc.accepted {
				t.Fatalf("accepted event = %v, want %v", hasAccepted(got.events), tc.accepted)
			}
			for _, event := range got.events {
				if event.GetAccepted() == nil && event.GetKeepalive() == nil {
					t.Errorf("received a queued or final event after the queue closed: %v", event)
				}
			}
			if tc.accepted {
				if got.err != nil {
					t.Fatalf("accepted Exec = %v, want OK", got.err)
				}
			} else {
				var ce *connect.Error
				if !errors.As(got.err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Message() != "the relay stream queue limit was reached" {
					t.Fatalf("unaccepted Exec = %v, want the queue limit diagnostic", got.err)
				}
			}
			peer.SendFrame(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{EchoRequested: true}))
			peer.WaitFrame("keepalive after both Exec calls ended", func(f *ownsandv1.DataFrame) bool {
				return f.GetRelayKeepalive() != nil
			})
			closes := 0
			for _, frame := range peer.Frames() {
				if isSlowClose(frame) {
					closes++
				}
			}
			if closes != 1 {
				t.Fatalf("the agent got %d close frames for the full stream, want one", closes)
			}
			t.Logf("full stream accepted=%v, status=%v, close frames=%d; concurrent Exec completed", tc.accepted, got.err, closes)
		})
	}
}

func TestRelayWriteFileQueueLimitBeforeCredit(t *testing.T) {
	limits := relay.DefaultLimits()
	limits.MaxQueuedFrames = 1
	c := startWriteCall(t, 15*time.Second, 0, func(o *app.Options) {
		o.RelayLimits = &limits
	})
	defer c.releaseWriteResult()
	for range limits.MaxQueuedFrames + 1 {
		c.relay.SendFrame(datapath.MustNewFrame(c.stream, c.sandboxID, &ownsandv1.WriteFileResponse{}))
	}
	err := c.waitDone(t, 3*time.Second)
	var ce *connect.Error
	if !errors.As(err, &ce) || ce.Code() != connect.CodeUnavailable || ce.Message() != "the relay stream queue limit was reached" {
		t.Fatalf("WriteFile = %v, want the queue limit diagnostic", err)
	}
	c.relay.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
		return f.GetStreamId() == c.stream && f.GetRelayStreamClose() != nil
	})
	t.Logf("WriteFile without send credit returned %v and closed its relay stream", err)
}
