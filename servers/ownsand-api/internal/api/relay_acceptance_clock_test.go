// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type acceptanceTrafficKind uint8

const (
	largeStreamFrame acceptanceTrafficKind = iota
	smallStreamFrame
	largeConnectionFrame
	largeClosedStreamFrame
	relayKeepalive
)

func TestRelayAcceptanceWallClockSteps(t *testing.T) {
	for _, tt := range []struct {
		name       string
		step       time.Duration
		stepBefore bool
		traffic    acceptanceTrafficKind
		accept     bool
	}{
		{name: "forward before receipt", step: 25 * time.Second, stepBefore: true, traffic: largeStreamFrame},
		{name: "backward before receipt", step: -25 * time.Second, stepBefore: true, traffic: largeStreamFrame},
		{name: "forward after receipt", step: 25 * time.Second, traffic: largeStreamFrame},
		{name: "backward after receipt", step: -25 * time.Second, traffic: largeStreamFrame},
		{name: "notice after original deadline", step: 25 * time.Second, traffic: largeStreamFrame, accept: true},
		{name: "small stream frame", traffic: smallStreamFrame},
		{name: "large connection frame", traffic: largeConnectionFrame},
		{name: "large closed-stream frame", traffic: largeClosedStreamFrame},
		{name: "keepalive", traffic: relayKeepalive},
	} {
		t.Run(tt.name, func(t *testing.T) {
			clock := testenv.NewClock()
			env := testenv.New(t, func(o *app.Options) {
				o.RelayClock = clock
				o.RelayTiming.Acceptance = 30 * time.Second
				o.RelayTiming.Keepalive = 40 * time.Millisecond
			})
			org, agent, sandboxID := runningSandbox(t, env)
			r := agent.ConnectRelay(nil)
			r.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })

			req := newExec(org.ID, sandboxID)
			grant, err := authorizeExec(t, org.OwnerClient, req)
			if err != nil {
				t.Fatal(err)
			}
			g := env.App.Plane.Grants.Get(grant.GetGrantId())
			if g == nil {
				t.Fatal("relay grant is missing")
			}
			conn := env.App.Plane.Relays.Get(g.DeviceID, g.ChannelGen)
			if conn == nil {
				t.Fatal("agent relay connection is missing")
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			other, err := conn.Open(ctx, "other-grant", sandboxID)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close(false)
			if tt.traffic == largeClosedStreamFrame {
				other.Close(false)
			}

			client := env.ClientsWith(testenv.UserToken(org.Owner), grantHeader(grant.GetGrantId())).Relay
			done := make(chan acceptanceResult, 1)
			go func() {
				es, err := client.Exec(ctx, connect.NewRequest(req))
				if err != nil {
					done <- acceptanceResult{err: err}
					return
				}
				events, err := collectExec(t, es)
				done <- acceptanceResult{events: events, err: err}
			}()
			forwarded := r.WaitFrame("forwarded exec", func(f *ownsandv1.DataFrame) bool {
				return f.GetExecRequest().GetExecId() == req.GetExecId()
			})
			// The writer must finish confirming the request before writing the
			// echo. Elapsed time stays at zero until we observe the echo.
			awaitAcceptanceEcho(r)

			clock.Advance(10 * time.Second)
			if tt.stepBefore {
				clock.StepWall(tt.step)
			}
			r.SendFrame(acceptanceTraffic(other, tt.traffic))
			// The receive loop must dispatch the traffic before the echo.
			awaitAcceptanceEcho(r)
			if tt.traffic == largeStreamFrame && !conn.LastLargeReceive().Equal(clock.Now()) {
				t.Fatal("the large-frame observer did not report the wall time of receipt")
			}
			clock.Advance(5 * time.Second)
			if !tt.stepBefore {
				clock.StepWall(tt.step)
			}

			deadline := 30 * time.Second
			if tt.traffic == largeStreamFrame {
				deadline = 40 * time.Second
			}
			if tt.accept {
				clock.Advance(35*time.Second - clock.Elapsed())
				assertAcceptancePending(t, done)
				r.SendFrame(datapath.MustNewFrame(forwarded.GetStreamId(), sandboxID, &ownsandv1.RequestAccepted{}))
				r.SendFrame(datapath.MustNewFrame(forwarded.GetStreamId(), sandboxID, &ownsandv1.ExecEvent{
					Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{
						Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0},
					}},
				}))
				result := <-done
				if result.err != nil || !hasAccepted(result.events) {
					t.Fatalf("exec at elapsed second 35 returned %v with accepted=%t", result.err, hasAccepted(result.events))
				}
				return
			}

			clock.Advance(deadline - time.Nanosecond - clock.Elapsed())
			assertAcceptancePending(t, done)
			clock.Advance(time.Nanosecond)
			result := <-done
			if testenv.Code(result.err) != connect.CodeUnavailable || hasAccepted(result.events) {
				t.Fatalf("exec at elapsed %v returned %v with accepted=%t", deadline, result.err, hasAccepted(result.events))
			}
			isClose := func(f *ownsandv1.DataFrame) bool {
				return f.GetStreamId() == forwarded.GetStreamId() && f.GetRelayStreamClose() != nil
			}
			r.WaitFrame("acceptance timeout stream close", isClose)
			// Open another stream as an outbound queue barrier before counting
			// closes. A late acceptance on the closed stream has no effect.
			r.SendFrame(datapath.MustNewFrame(forwarded.GetStreamId(), sandboxID, &ownsandv1.RequestAccepted{}))
			barrier, err := conn.Open(ctx, "barrier-grant", sandboxID)
			if err != nil {
				t.Fatal(err)
			}
			defer barrier.Close(false)
			r.WaitFrame("stream open after timeout", func(f *ownsandv1.DataFrame) bool {
				return f.GetStreamId() == barrier.ID && f.GetRelayStreamOpen() != nil
			})
			closeCount := 0
			for _, f := range r.Frames() {
				if isClose(f) {
					closeCount++
				}
			}
			if closeCount != 1 {
				t.Fatalf("stream close count = %d, want 1", closeCount)
			}
		})
	}
}

func acceptanceTraffic(other *relay.Stream, traffic acceptanceTrafficKind) *ownsandv1.DataFrame {
	switch traffic {
	case largeConnectionFrame:
		kinds := datapath.AllKinds()
		return datapath.MustNewFrame(0, "", &ownsandv1.SupportedFrameKinds{
			Kinds: slices.Repeat(kinds, 1+relay.LargeFrameBytes/len(kinds)),
		})
	case relayKeepalive:
		return datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
	default:
		size := relay.LargeFrameBytes
		if traffic == smallStreamFrame {
			size = 1
		}
		return datapath.MustNewFrame(other.ID, other.SandboxID, &ownsandv1.ExecEvent{
			Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, size)}},
		})
	}
}

func awaitAcceptanceEcho(r *testenv.FakeRelay) {
	keepalives := 0
	for _, f := range r.Frames() {
		if f.GetRelayKeepalive() != nil {
			keepalives++
		}
	}
	r.SendFrame(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{EchoRequested: true}))
	r.WaitFrame("keepalive echo", func(f *ownsandv1.DataFrame) bool {
		if f.GetRelayKeepalive() == nil {
			return false
		}
		keepalives--
		return keepalives < 0
	})
}

func assertAcceptancePending(t *testing.T, done <-chan acceptanceResult) {
	t.Helper()
	select {
	case r := <-done:
		t.Fatalf("the call ended before the elapsed deadline: %v", r.err)
	case <-time.After(100 * time.Millisecond):
		// The acceptance handler checks the deadline every 10 ms.
	}
}
