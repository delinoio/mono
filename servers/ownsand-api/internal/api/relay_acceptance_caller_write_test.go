// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/relay"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type stalledCallerWriter struct {
	http.ResponseWriter
	entered chan<- struct{}
	resume  <-chan struct{}
	ctx     context.Context
	once    sync.Once
}

func (w *stalledCallerWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	select {
	case <-w.resume:
		return w.ResponseWriter.Write(p)
	case <-w.ctx.Done():
		return 0, w.ctx.Err()
	}
}

func (w *stalledCallerWriter) Flush() { w.ResponseWriter.(http.Flusher).Flush() }

func (w *stalledCallerWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// TestRelayAcceptanceWhileCallerWriteStalls holds the first caller keepalive
// write while the agent grants credit, receives the forwarded request and
// replies over a separate HTTP/2 connection. In every case the handler
// resumes after the acceptance window from send completion has expired.
func TestRelayAcceptanceWhileCallerWriteStalls(t *testing.T) {
	const acceptance = time.Second
	accepted := []proto.Message{
		&ownsandv1.RequestAccepted{},
		&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Exited{Exited: &ownsandv1.ExecExited{
			Status: &ownsandv1.ExecExited_ExitCode{ExitCode: 0},
		}}},
	}
	for _, tc := range []struct {
		name         string
		reply        []proto.Message
		late         bool
		extend       bool
		wantAccepted bool
		wantCode     connect.Code
		wantCloses   int
	}{
		{name: "timely notice", reply: accepted, wantAccepted: true, wantCloses: 1},
		{name: "timely notice after extension", reply: accepted, extend: true, wantAccepted: true, wantCloses: 1},
		{name: "late notice", reply: accepted, late: true, wantCode: connect.CodeUnavailable, wantCloses: 1},
		{name: "silent agent", wantCode: connect.CodeUnavailable, wantCloses: 1},
		{name: "timely terminal error", reply: []proto.Message{&ownsandv1.ErrorDetail{
			Code: ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, Message: "test terminal error",
		}}, wantCode: connect.CodeFailedPrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := testenv.New(t, func(o *app.Options) {
				o.RelayTiming.Acceptance = acceptance
				o.RelayTiming.Rotation = 30 * time.Second
				o.RelayTiming.RequestDeadline = 10 * time.Second
				o.RelayTiming.Keepalive = 20 * time.Millisecond
			})
			org, agent, sandboxID := runningSandbox(t, env)
			entered, resume := make(chan struct{}), make(chan struct{})
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				env.App.Handler().ServeHTTP(&stalledCallerWriter{
					ResponseWriter: w, entered: entered, resume: resume, ctx: r.Context(),
				}, r)
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()

			peer := env.Clients(env.SessionCredential(agent.Device)).RelayAgent.Connect(ctx)
			send := func(streamID uint64, sandboxID string, payload proto.Message) {
				t.Helper()
				if err := peer.Send(&ownsandv1.ConnectRequest{Frame: datapath.MustNewFrame(streamID, sandboxID, payload)}); err != nil {
					t.Fatal(err)
				}
			}
			closes := 0
			receive := func(match func(*ownsandv1.DataFrame) bool) *ownsandv1.DataFrame {
				t.Helper()
				for {
					r, err := peer.Receive()
					if err != nil {
						t.Fatal(err)
					}
					f := r.GetFrame()
					if f.GetRelayStreamClose() != nil {
						closes++
					}
					if match(f) {
						return f
					}
				}
			}
			echo := func() {
				send(0, "", &ownsandv1.RelayKeepalive{EchoRequested: true})
				receive(func(f *ownsandv1.DataFrame) bool { return f.GetRelayKeepalive() != nil })
			}
			send(0, "", &ownsandv1.RelayKeepalive{})
			receive(func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })

			req := newExec(org.ID, sandboxID)
			grant, err := authorizeExec(t, org.OwnerClient, req)
			if err != nil {
				t.Fatal(err)
			}
			var fillerID uint64
			if tc.extend {
				plane := env.App.Plane
				g := plane.Grants.Get(grant.GetGrantId())
				if g == nil {
					t.Fatal("the grant is gone")
				}
				conn := plane.Relays.Get(g.DeviceID, g.ChannelGen)
				if conn == nil {
					t.Fatal("the relay connection is gone")
				}
				filler, err := conn.Open(ctx, "filler-grant", sandboxID)
				if err != nil {
					t.Fatal(err)
				}
				fillerID = filler.ID
				receive(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen() != nil && f.GetStreamId() == fillerID })
			}
			client := ownsandv1connect.NewRelayServiceClient(srv.Client(), srv.URL)
			done := make(chan acceptanceResult, 1)
			go func() {
				r := connect.NewRequest(req)
				r.Header().Set("Authorization", "Bearer "+testenv.UserToken(org.Owner))
				r.Header().Set(api.RelayGrantHeader, grant.GetGrantId())
				stream, err := client.Exec(ctx, r)
				if err != nil {
					done <- acceptanceResult{err: err}
					return
				}
				events, err := collectExec(t, stream)
				done <- acceptanceResult{events: events, err: err}
			}()
			open := receive(func(f *ownsandv1.DataFrame) bool { return f.GetRelayStreamOpen().GetGrantId() == grant.GetGrantId() })
			select {
			case <-entered:
			case result := <-done:
				t.Fatalf("Exec ended before its caller write stalled: %v", result.err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			// The response write is stalled before the agent grants credit,
			// so the handler cannot observe forwarding completion when it happens.
			send(open.GetStreamId(), sandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: datapath.MinWindowSize})
			receive(func(f *ownsandv1.DataFrame) bool { return f.GetExecRequest().GetExecId() == req.GetExecId() })
			afterDeadline := time.NewTimer(2 * acceptance)
			defer afterDeadline.Stop()
			if tc.late {
				<-afterDeadline.C
			}
			if tc.extend {
				time.Sleep(3 * acceptance / 4)
				send(fillerID, sandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{
					Stdout: &ownsandv1.ExecOutput{Data: make([]byte, relay.LargeFrameBytes)},
				}})
				echo()
				time.Sleep(acceptance / 2)
			}
			for _, payload := range tc.reply {
				send(open.GetStreamId(), sandboxID, payload)
			}
			echo() // The relay reader has dispatched every reply before the echo.
			if !tc.late {
				<-afterDeadline.C
			}
			close(resume)

			result := <-done
			if got := hasAccepted(result.events); got != tc.wantAccepted {
				t.Errorf("accepted event = %v, want %v", got, tc.wantAccepted)
			}
			if got := connect.CodeOf(result.err); (tc.wantCode == 0 && result.err != nil) || (tc.wantCode != 0 && got != tc.wantCode) {
				t.Errorf("Exec = %v, want code %v", result.err, tc.wantCode)
			}
			if tc.wantCode == connect.CodeUnavailable {
				if e, ok := result.err.(*connect.Error); !ok || e.Message() != "the agent did not accept the request" {
					t.Errorf("Exec = %v, want the acceptance timeout diagnostic", result.err)
				}
			}
			echo() // Any close was queued before the caller received the RPC status.
			if closes != tc.wantCloses {
				t.Errorf("agent received %d close frames, want %d", closes, tc.wantCloses)
			}
			t.Logf("accepted=%v, status=%v, stream closes=%d", hasAccepted(result.events), result.err, closes)
		})
	}
}
