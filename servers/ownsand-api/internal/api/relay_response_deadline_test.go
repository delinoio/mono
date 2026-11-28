// SPDX-License-Identifier: FSL-1.1-ALv2

package api_test

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
	"github.com/delinoio/mono/servers/ownsand-api/internal/api"
	"github.com/delinoio/mono/servers/ownsand-api/internal/app"
	"github.com/delinoio/mono/servers/ownsand-api/internal/testenv"
)

type relayStall int

const (
	noRelayStall relayStall = iota
	stallRelayWrite
	stallRelayFlush
	stallRelayFinalStatus
)

// relayDeadlineWriter holds the configured write or flush until the real
// HTTP/2 transport closes that response, or the test releases it.
type relayDeadlineWriter struct {
	http.ResponseWriter
	ctx     context.Context
	stall   relayStall
	entered chan<- struct{}
	release <-chan struct{}
	onArm   func(time.Time)
	blocked bool
}

func (w *relayDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	err := http.NewResponseController(w.ResponseWriter).SetWriteDeadline(deadline)
	if err == nil && !deadline.IsZero() {
		w.onArm(deadline)
	}
	return err
}

func (w *relayDeadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *relayDeadlineWriter) hold() error {
	w.blocked = true
	close(w.entered)
	select {
	case <-w.release:
		return nil
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func (w *relayDeadlineWriter) Write(p []byte) (int, error) {
	// Connect writes the five-byte envelope prefix separately from its body.
	// Bit 1 marks the end-of-stream message, which carries the RPC status.
	finalStatus := len(p) == 5 && p[0]&0x02 != 0
	if !w.blocked && (w.stall == stallRelayWrite || (w.stall == stallRelayFinalStatus && finalStatus)) {
		if err := w.hold(); err != nil {
			return 0, err
		}
	}
	return w.ResponseWriter.Write(p)
}

func (w *relayDeadlineWriter) Flush() {
	if !w.blocked && w.stall == stallRelayFlush {
		if err := w.hold(); err != nil {
			return
		}
	}
	w.ResponseWriter.(http.Flusher).Flush()
}

func relayRequestWithGrant[T any](token, grantID string, message *T) *connect.Request[T] {
	r := connect.NewRequest(message)
	r.Header().Set("Authorization", "Bearer "+token)
	r.Header().Set(api.RelayGrantHeader, grantID)
	return r
}

func TestRelayResponseWriteDeadline(t *testing.T) {
	t.Parallel()
	const (
		rotation = 450 * time.Millisecond
		hard     = 1800 * time.Millisecond
	)
	env := testenv.New(t, func(o *app.Options) {
		o.RelayTiming.Rotation = rotation
		o.RelayTiming.ResponseWriteDeadline = hard
		o.RelayTiming.Keepalive = 80 * time.Millisecond
	})
	org, agent, sandboxID := runningSandbox(t, env)
	peer := agent.ConnectRelay(nil)
	peer.WaitFrame("supported frame kinds", func(f *ownsandv1.DataFrame) bool { return f.GetSupportedFrameKinds() != nil })
	token := testenv.UserToken(org.Owner)

	for _, tc := range []struct {
		name        string
		stall       relayStall
		wantReset   bool
		terminal    bool
		callerClose bool
		rpcTimeout  bool
	}{
		{name: "write", stall: stallRelayWrite, wantReset: true},
		{name: "flush", stall: stallRelayFlush, wantReset: true},
		{name: "final status", stall: stallRelayFinalStatus, wantReset: true},
		{name: "rotation"},
		{name: "terminal event", terminal: true},
		{name: "caller cancellation", stall: stallRelayWrite, callerClose: true},
		{name: "earlier request deadline", stall: stallRelayWrite, rpcTimeout: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newExec(org.ID, sandboxID)
			grant, err := authorizeExec(t, org.OwnerClient, req)
			if err != nil {
				t.Fatal(err)
			}
			peer.SetOnRequest(func(s *testenv.RelayStream, f *ownsandv1.DataFrame) {
				if tc.terminal || f.GetExecRequest().GetExecId() != req.GetExecId() {
					serveExec(s, f)
					return
				}
				s.Send(&ownsandv1.RequestAccepted{})
				s.Send(&ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{
					Stdout: &ownsandv1.ExecOutput{Data: []byte("before rotation")},
				}})
			})
			entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			armed := make(chan time.Time, 1)
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get(api.RelayGrantHeader) == grant.GetGrantId() {
					defer close(returned)
					w = &relayDeadlineWriter{
						ResponseWriter: w, ctx: r.Context(), stall: tc.stall, entered: entered, release: release,
						onArm: func(deadline time.Time) {
							select {
							case armed <- deadline:
							default:
							}
						},
					}
				}
				env.App.Handler().ServeHTTP(w, r)
			}))
			srv.EnableHTTP2 = true
			srv.StartTLS()
			defer srv.Close()

			budget, stop := context.WithTimeout(t.Context(), 8*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			client := ownsandv1connect.NewRelayServiceClient(srv.Client(), srv.URL)
			run := func(ctx context.Context, req *ownsandv1.ExecRequest, grantID string) ([]*ownsandv1.ExecEvent, error) {
				r := relayRequestWithGrant(token, grantID, req)
				if tc.rpcTimeout && grantID == grant.GetGrantId() {
					r.Header().Set("Connect-Timeout-Ms", "400")
				}
				stream, err := client.Exec(ctx, r)
				if err != nil {
					return nil, err
				}
				return collectExec(t, stream)
			}

			gotConn := make(chan net.Conn, 1)
			ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
				GotConn: func(info httptrace.GotConnInfo) { gotConn <- info.Conn },
			})
			done := make(chan acceptanceResult, 1)
			started := time.Now()
			go func() {
				events, err := run(ctx, req, grant.GetGrantId())
				done <- acceptanceResult{events: events, err: err}
			}()
			var deadline time.Time
			select {
			case deadline = <-armed:
				if allocated := deadline.Add(-hard); allocated.Before(started) || allocated.After(time.Now()) {
					t.Fatalf("deadline %v is outside the allocation window", deadline)
				}
			case <-budget.Done():
				t.Fatal("relay handler did not arm the deadline")
			}

			open := peer.WaitFrame("stream open", func(f *ownsandv1.DataFrame) bool {
				return f.GetRelayStreamOpen().GetGrantId() == grant.GetGrantId()
			})
			waitForClose := func() {
				peer.WaitFrame("stream close", func(f *ownsandv1.DataFrame) bool {
					return f.GetStreamId() == open.GetStreamId() && f.GetRelayStreamClose() != nil
				})
			}
			if tc.stall != noRelayStall {
				select {
				case <-entered:
				case <-returned:
					t.Fatal("relay handler returned before the writer stalled")
				case <-budget.Done():
					t.Fatal("relay response did not stall")
				}
			}
			if tc.stall == stallRelayFinalStatus {
				// The relay has cleaned up at rotation. Connect is still
				// writing the final status under the same deadline.
				waitForClose()
				select {
				case <-returned:
					t.Fatal("Connect finalization did not remain blocked")
				default:
				}
			}

			if tc.wantReset && tc.stall != stallRelayFlush {
				other := newExec(org.ID, sandboxID)
				otherGrant, err := authorizeExec(t, org.OwnerClient, other)
				if err != nil {
					t.Fatal(err)
				}
				otherConn := make(chan net.Conn, 1)
				otherCtx := httptrace.WithClientTrace(budget, &httptrace.ClientTrace{
					GotConn: func(info httptrace.GotConnInfo) { otherConn <- info.Conn },
				})
				events, err := run(otherCtx, other, otherGrant.GetGrantId())
				select {
				case <-returned:
					t.Fatal("first response ended before the second response completed")
				default:
				}
				if err != nil || len(events) == 0 || events[len(events)-1].GetExited() == nil || <-otherConn != <-gotConn {
					t.Fatalf("second response did not finish on the same connection: %d events, error %v", len(events), err)
				}
			}

			if tc.callerClose {
				cancel()
			}
			if tc.rpcTimeout {
				// The RPC header expires before the transport deadline.
				// Release the write after the earlier deadline has passed.
				time.Sleep(time.Until(deadline.Add(-hard + 700*time.Millisecond)))
				close(release)
			}

			select {
			case <-returned:
			case <-budget.Done():
				t.Fatal("relay HTTP handler did not return")
			}
			var result acceptanceResult
			select {
			case result = <-done:
			case <-budget.Done():
				t.Fatal("caller did not observe the end of the response")
			}
			elapsed := time.Since(deadline.Add(-hard))
			switch {
			case tc.wantReset:
				if result.err == nil || elapsed < hard || elapsed > hard+2*time.Second {
					t.Fatalf("stalled response returned %v after %v, want a transport error at %v", result.err, elapsed, hard)
				}
			case tc.callerClose || tc.rpcTimeout:
				want := connect.CodeCanceled
				if tc.rpcTimeout {
					want = connect.CodeDeadlineExceeded
				}
				if connect.CodeOf(result.err) != want || elapsed >= hard {
					t.Fatalf("caller returned %v after %v, want %v before the hard deadline", result.err, elapsed, want)
				}
				// Also check the handler outcome. A canceled client can
				// return its own context error before receiving a status.
				logged := false
				for _, line := range strings.Split(env.Logs.String(), "\n") {
					if strings.Contains(line, `"msg":"relay_rpc"`) && strings.Contains(line, fmt.Sprintf(`"grant_id":%q`, grant.GetGrantId())) {
						logged = strings.Contains(line, fmt.Sprintf(`"code":%q`, want.String()))
						break
					}
				}
				if !logged {
					t.Fatalf("relay handler did not report %v", want)
				}
			default:
				if result.err != nil || !hasAccepted(result.events) {
					t.Fatalf("writable response: accepted=%v, error=%v", hasAccepted(result.events), result.err)
				}
				last := result.events[len(result.events)-1]
				if tc.terminal && last.GetExited() == nil || !tc.terminal && (last.GetKeepalive() == nil || elapsed < rotation || elapsed >= hard) {
					t.Fatalf("writable response ended after %v with event %T", elapsed, last.GetEvent())
				}
			}
			waitForClose()
			closes := 0
			for _, f := range peer.Frames() {
				if f.GetStreamId() == open.GetStreamId() && f.GetRelayStreamClose() != nil {
					closes++
				}
			}
			if closes != 1 {
				t.Fatalf("agent received %d closes for the response, want 1", closes)
			}
			t.Logf("HTTP/2 response returned after %v with status %v and released its relay stream", elapsed, result.err)
		})
	}
}
