// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func relayStdout(id uint64, data string) *ownsandv1.DataFrame {
	return datapath.MustNewFrame(id, "sandbox", &ownsandv1.ExecEvent{
		Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte(data)}},
	})
}

func TestRelayStreamEndRechecksAdmissionAfterQueueWake(t *testing.T) {
	for _, inbound := range []struct {
		name  string
		frame *ownsandv1.DataFrame
	}{
		{"revocation", nil},
		{"close", closeFrame(1)},
		{"error", datapath.MustNewFrame(1, "sandbox", &ownsandv1.ErrorDetail{
			Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED,
		})},
	} {
		t.Run(inbound.name, func(t *testing.T) {
			for _, notice := range []string{"payload", "acceptance"} {
				t.Run(notice, func(t *testing.T) {
					f, g, st := blockedSendFixture(t)
					probe := newWaitProbeContext(st.ctx)
					resume := make(chan struct{})
					probe.resume = resume
					unpause := sync.OnceFunc(func() { close(resume) })
					t.Cleanup(unpause)
					st.ctx = probe
					var returned <-chan struct{}
					var sendResult chan error
					if notice == "acceptance" {
						d := &acceptNoticeDispatcher{probe: probe, accepted: make(chan struct{})}
						f.r.d.opts.Dispatcher = d
						req := &ownsandv1.ExecRequest{OrganizationId: orgID, SandboxId: g.sandboxID, ExecId: newID(t), Command: []string{"/bin/true"}}
						f.r.onFrame(f.c, datapath.MustNewFrame(st.id, g.sandboxID, req))
						returned = d.accepted
					} else {
						done := make(chan struct{})
						sendResult = make(chan error, 1)
						probe.arm()
						go func() {
							defer close(done)
							sendResult <- (relaySink{f.r, st}).Send(st.ctx, relayStdout(st.id, "late"))
						}()
						returned = done
					}

					select {
					case <-probe.waited:
					case <-time.After(5 * time.Second):
						t.Fatal("sender did not reach the full queue")
					}
					// Pause before the wait selects a case. Make both capacity
					// and cancellation ready before the sender can continue.
					<-f.c.out
					f.r.outboundDequeued(f.c)
					if inbound.frame == nil {
						f.r.revoke(g.id)
						select {
						case frame := <-f.c.out:
							if frame.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
								t.Fatalf("queued frame %v, want revocation", frame)
							}
						default:
							t.Fatal("the revocation did not queue a terminal frame")
						}
					} else {
						f.r.onFrame(f.c, inbound.frame)
					}
					unpause()
					select {
					case <-returned:
					case <-time.After(5 * time.Second):
						t.Fatal("sender did not return after stream end")
					}
					if sendResult != nil {
						if err := <-sendResult; !errors.Is(err, context.Canceled) {
							t.Fatalf("send returned %v, want context.Canceled", err)
						}
					}
					if len(f.c.out) != 0 || f.c.ctx.Err() != nil {
						t.Fatalf("queue holds %d frames, connection context error %v", len(f.c.out), f.c.ctx.Err())
					}
				})
			}
		})
	}
}

func TestRelayTerminalFollowsQueuedFrames(t *testing.T) {
	for _, action := range []string{"revocation", "protocol error", "dispatcher error"} {
		t.Run(action, func(t *testing.T) {
			f := newRevokeFixture(t, 4)
			g, streams := f.grant(time.Hour, time.Now(), 1)
			st := streams[0]
			st.send, _ = datapath.NewSendWindow(1 << 20)
			notice := datapath.MustNewFrame(st.id, g.sandboxID, &ownsandv1.RequestAccepted{})
			output := relayStdout(st.id, "before")
			for _, frame := range []*ownsandv1.DataFrame{notice, output} {
				if err := (relaySink{f.r, st}).Send(st.ctx, frame); err != nil {
					t.Fatal(err)
				}
			}
			invalid := &ownsandv1.DataFrame{StreamId: st.id, SandboxId: g.sandboxID, Kind: ownsandv1.DataFrameKind(99)}
			switch action {
			case "revocation":
				f.r.revoke(g.id)
			case "protocol error":
				f.r.onFrame(f.c, invalid)
			case "dispatcher error":
				detail := &ownsandv1.ErrorDetail{Code: ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE}
				if err := (relaySink{f.r, st}).Send(st.ctx, datapath.MustNewFrame(st.id, g.sandboxID, detail)); err != nil {
					t.Fatal(err)
				}
				// The dispatcher has queued its final response and has
				// yet to return its outcome when revocation arrives.
				f.r.revoke(g.id)
			}
			f.r.onFrame(f.c, invalid) // A late invalid frame must not elicit a second terminal.
			if err := (relaySink{f.r, st}).Send(st.ctx, relayStdout(st.id, "late")); !errors.Is(err, context.Canceled) {
				t.Fatalf("send after termination returned %v, want context.Canceled", err)
			}
			if n := len(f.c.out); n != 3 || f.c.ctx.Err() != nil {
				t.Fatalf("queue holds %d frames, want 3 with connection active", n)
			}
			if first, second := <-f.c.out, <-f.c.out; first != notice || second != output {
				t.Fatalf("queued frames out of order: %v, %v", first, second)
			}
			terminal := <-f.c.out
			switch action {
			case "revocation":
				if terminal.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
					t.Fatalf("last frame %v, want revocation", terminal)
				}
			case "protocol error":
				if terminal.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED {
					t.Fatalf("last frame %v, want protocol error", terminal)
				}
			case "dispatcher error":
				if terminal.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE {
					t.Fatalf("last frame %v, want dispatcher error", terminal)
				}
			}
		})
	}
}

func TestRelayFullQueueTerminationReleasesWaitingSenders(t *testing.T) {
	for _, action := range []string{"revocation", "protocol error"} {
		t.Run(action, func(t *testing.T) {
			f, g, st := blockedSendFixture(t)
			_, others := f.grant(time.Hour, time.Now(), 2)
			results := make(chan error, 2)
			for _, stream := range []*rstream{st, others[0]} {
				probe := newWaitProbeContext(stream.ctx)
				stream.ctx = probe
				probe.arm()
				go func() {
					results <- (relaySink{f.r, stream}).Send(stream.ctx,
						datapath.MustNewFrame(stream.id, g.sandboxID, &ownsandv1.RequestAccepted{}))
				}()
				select {
				case <-probe.waited:
				case <-time.After(5 * time.Second):
					t.Fatal("sender did not reach the full queue")
				}
			}

			within(t, action, func() {
				if action == "revocation" {
					f.r.revoke(g.id)
					return
				}
				f.r.onFrame(f.c, &ownsandv1.DataFrame{StreamId: st.id, SandboxId: g.sandboxID, Kind: ownsandv1.DataFrameKind(99)})
			})
			for range 2 {
				select {
				case err := <-results:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("blocked sender returned %v, want context.Canceled", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("connection close did not release a blocked sender")
				}
			}
			if len(f.c.out) != 1 || f.c.ctx.Err() == nil {
				t.Fatalf("queue holds %d frames, want original frame on canceled connection", len(f.c.out))
			}
		})
	}
}

// This exercises the agent data plane over loopback HTTP/2 with the real
// process-runtime guest. A second exec lets the original command print its
// final output only after the first relay stream has ended.
func TestRelayTerminalOrderWithProcessGuest(t *testing.T) {
	for _, action := range []string{"revocation", "close", "error"} {
		t.Run(action, func(t *testing.T) {
			e := newRelayEnv(t, Timing{}, RelayLimits{})
			req := e.execReq("/bin/sh", "-c", "printf 'before\\n'; while [ ! -f relay-resume ]; do sleep 0.05; done; printf 'after\\n'")
			g := e.execGrant(req)
			rc := e.conn()
			e.openOK(rc, 1, g.GetGrantId())
			rc.send(datapath.MustNewFrame(1, e.sandbox, req))
			if f := rc.nextOn(1); f.GetRequestAccepted() == nil {
				t.Fatalf("first frame after window: %v, want acceptance", f)
			}
			if f := rc.nextOn(1); f.GetExecEvent().GetStarted() == nil {
				t.Fatalf("first frame after acceptance: %v, want exec started", f)
			}
			if f := rc.nextOn(1); string(f.GetExecEvent().GetStdout().GetData()) != "before\n" {
				t.Fatalf("initial output: %v, want before", f)
			}

			switch action {
			case "revocation":
				e.revokeGrant(g)
				if f := rc.nextOn(1); f.GetSessionEnd().GetReason() != ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED {
					t.Fatalf("revoked stream ended with %v", f)
				}
			case "close":
				rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.RelayStreamClose{}))
			case "error":
				rc.send(datapath.MustNewFrame(1, e.sandbox, &ownsandv1.ErrorDetail{
					Code: ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED,
				}))
			}
			waitFor(t, "first stream removal", func() bool {
				r := e.dp.relay
				r.mu.Lock()
				defer r.mu.Unlock()
				return r.conn != nil && r.conn.streams[1] == nil
			})

			// Read every frame until the attached exec exits, so an
			// unexpected reply or output on stream 1 cannot be skipped.
			nextOn := func(id uint64) *ownsandv1.DataFrame {
				t.Helper()
				for {
					f := rc.next()
					if f.GetStreamId() == 1 {
						t.Fatalf("ended stream queued a late frame: %v", f)
					}
					if f.GetStreamId() == id && f.GetExecEvent().GetKeepalive() == nil {
						return f
					}
				}
			}
			run := func(id uint64, request *ownsandv1.DataFrame, grantID, want string) {
				t.Helper()
				e.open(rc, id, grantID, 64<<20)
				if f := nextOn(id); f.GetRelayWindowUpdate() == nil {
					t.Fatalf("stream %d did not open: %v", id, f)
				}
				rc.send(request)
				if f := nextOn(id); f.GetRequestAccepted() == nil {
					t.Fatalf("stream %d was not accepted: %v", id, f)
				}
				var output string
				for {
					f := nextOn(id)
					ev := f.GetExecEvent()
					if ev == nil || ev.GetFailed() != nil || ev.GetSandboxEnded() != nil {
						t.Fatalf("stream %d failed: %v", id, f)
					}
					output += string(ev.GetStdout().GetData())
					if exit := ev.GetExited(); exit != nil {
						if exit.GetExitCode() != 0 || output != want {
							t.Fatalf("stream %d exit %v with output %q, want %q", id, exit, output, want)
						}
						return
					}
				}
			}

			other := e.execReq("/bin/sh", "-c", "touch relay-resume && printf 'other\\n'")
			run(2, datapath.MustNewFrame(2, e.sandbox, other), e.execGrant(other).GetGrantId(), "other\n")
			attach := &ownsandv1.AttachExecRequest{OrganizationId: orgID, SandboxId: e.sandbox, ExecId: req.GetExecId()}
			attachGrant, ack := e.grant(ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC, req.GetExecId(), nil, time.Minute, time.Minute, "")
			if ack.GetResult() != ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED {
				t.Fatalf("attach grant: %v", ack)
			}
			run(3, datapath.MustNewFrame(3, e.sandbox, attach), attachGrant.GetGrantId(), "before\nafter\n")
		})
	}
}
