// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/ops"
	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// blockingDispatcher holds every request until its stream is released or its
// context ends, and records the streams that reached it.
type blockingDispatcher struct {
	mu      sync.Mutex
	started []uint64
	release map[uint64]chan struct{}
}

func newBlockingDispatcher() *blockingDispatcher {
	return &blockingDispatcher{release: map[uint64]chan struct{}{}}
}

func (b *blockingDispatcher) gate(id uint64) chan struct{} {
	b.mu.Lock()
	defer b.mu.Unlock()
	g := b.release[id]
	if g == nil {
		g = make(chan struct{})
		b.release[id] = g
	}
	return g
}

func (b *blockingDispatcher) Handle(ctx context.Context, req ops.Request, _ ops.Sink) ops.Outcome {
	id := req.Frame.GetStreamId()
	g := b.gate(id)
	b.mu.Lock()
	b.started = append(b.started, id)
	b.mu.Unlock()
	select {
	case <-g:
	case <-ctx.Done():
	}
	return ops.Outcome{}
}

func (b *blockingDispatcher) ExecEnded(string) bool { return true }

func (b *blockingDispatcher) releaseStream(id uint64) { close(b.gate(id)) }

func (b *blockingDispatcher) startedStreams() []uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]uint64(nil), b.started...)
}

func (s *session) openStreams() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.streams)
}

// limitedSession opens one session on a data plane with the given limits and
// a blocking dispatcher.
func limitedSession(t *testing.T, w *world, limits SessionLimits, timing Timing) (*DataPlane, *blockingDispatcher, *caller, *session) {
	t.Helper()
	disp := newBlockingDispatcher()
	dp := New(Options{Dispatcher: disp, Sandboxes: w.m, Logger: logger(), Timing: timing, SessionLimits: limits, IncludeLoopback: true, InterfaceFilter: loopbackOnly})
	t.Cleanup(dp.Close)
	ch := newChannel(1)
	dp.ChannelOpened(ch)
	c, id := w.session(t, dp, ch, time.Hour)
	dp.mu.Lock()
	s := dp.sessions[id]
	dp.mu.Unlock()
	if s == nil {
		t.Fatal("session not registered")
	}
	return dp, disp, c, s
}

func requireStreamRejected(t *testing.T, c *caller, id uint64) {
	t.Helper()
	f := c.next()
	if f.GetStreamId() != id {
		t.Fatalf("frame on stream %d, want %d", f.GetStreamId(), id)
	}
	if got := f.GetError().GetCode(); got != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("frame %v, want a resource_exhausted error", f)
	}
	if f.GetError().GetMessage() != "agent peer-to-peer stream capacity reached" {
		t.Fatalf("message %q", f.GetError().GetMessage())
	}
}

// A request on a new stream beyond the limit gets one resource_exhausted
// frame, registers nothing and reaches no dispatcher; the session stays open
// and takes a request again once a stream ended.
func TestSessionStreamLimit(t *testing.T) {
	w := newWorld(t)
	_, disp, c, s := limitedSession(t, w, SessionLimits{MaxStreamsPerSession: 2}, Timing{})
	c.send(datapath.MustNewFrame(1, w.sandbox, w.execReq("/bin/true")))
	c.send(datapath.MustNewFrame(2, w.sandbox, w.execReq("/bin/true")))
	waitFor(t, "two open streams", func() bool { return len(disp.startedStreams()) == 2 })

	c.send(datapath.MustNewFrame(3, w.sandbox, w.execReq("/bin/true")))
	requireStreamRejected(t, c, 3)
	if n := s.openStreams(); n != 2 {
		t.Fatalf("%d open streams after a rejection, want 2", n)
	}
	if got := disp.startedStreams(); len(got) != 2 {
		t.Fatalf("dispatcher saw streams %v, want only the two admitted ones", got)
	}
	if ended, _ := s.endState(); ended {
		t.Fatal("a rejected stream ended the session")
	}

	disp.releaseStream(1)
	waitFor(t, "one open stream", func() bool { return s.openStreams() == 1 })
	c.send(datapath.MustNewFrame(4, w.sandbox, w.execReq("/bin/true")))
	waitFor(t, "stream 4 to reach the dispatcher", func() bool { return len(disp.startedStreams()) == 3 })
	if got := disp.startedStreams(); got[2] != 4 {
		t.Fatalf("dispatcher saw streams %v, want stream 4 last", got)
	}
}

// A rejected stream holds no slot, so it does not keep the session out of the
// idle rule.
func TestSessionStreamLimitKeepsIdleRule(t *testing.T) {
	w := newWorld(t)
	_, disp, c, s := limitedSession(t, w, SessionLimits{MaxStreamsPerSession: 1}, Timing{SessionIdle: time.Hour})
	c.send(datapath.MustNewFrame(1, w.sandbox, w.execReq("/bin/true")))
	waitFor(t, "one open stream", func() bool { return len(disp.startedStreams()) == 1 })
	c.send(datapath.MustNewFrame(2, w.sandbox, w.execReq("/bin/true")))
	requireStreamRejected(t, c, 2)

	// The admitted request keeps the quiet session open.
	s.setActivity(time.Now().Add(-2 * time.Hour))
	if s.endIdle() {
		t.Fatal("a session with an open request ended as idle")
	}
	disp.releaseStream(1)
	waitFor(t, "no open stream", func() bool { return s.openStreams() == 0 })
	// The rejected stream left nothing behind; the session is idle again once
	// the finished request's activity is old.
	s.setActivity(time.Now().Add(-2 * time.Hour))
	if !s.endIdle() {
		t.Fatal("a rejected stream kept the session out of the idle rule")
	}
	if ended, reason := s.endState(); !ended || reason != ownsandv1.SessionEndReason_SESSION_END_REASON_IDLE {
		t.Fatalf("ended %v reason %v", ended, reason)
	}
	c.waitClosed()
}

func requireSessionCapacityAnswer(t *testing.T, a *ownsandv1.SignalingAnswer, id string) {
	t.Helper()
	if a.GetSessionId() != id || a.GetSdp() != "" {
		t.Fatalf("answer %v, want an error answer for session %s without an SDP body", a, id)
	}
	if got := a.GetError().GetCode(); got != ownsandv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED {
		t.Fatalf("answer error code %v, want resource_exhausted", got)
	}
}

// An offer beyond the session limit gets a resource_exhausted answer without
// an SDP body and leaves no session; the sessions that exist stay open and
// the slot of an ended session is free again.
func TestOfferSessionLimit(t *testing.T) {
	w := newWorld(t)
	dp, _, c, s := limitedSession(t, w, SessionLimits{MaxSessions: 1}, Timing{})
	ch := newChannel(1)
	_, sdp := newCaller(t, DataChannelLabel)
	id := newID(t)
	a := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp})
	requireSessionCapacityAnswer(t, a, id)
	if n := dp.sessionCount(); n != 1 {
		t.Fatalf("%d sessions, want 1", n)
	}
	if ended, _ := s.endState(); ended {
		t.Fatal("a rejected offer ended the existing session")
	}
	// A duplicate session ID is still an invalid offer, not a capacity error.
	dup := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: s.id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp})
	if got := dup.GetError().GetCode(); got != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
		t.Fatalf("duplicate answer %v", dup)
	}

	s.end(ownsandv1.SessionEndReason_SESSION_END_REASON_REVOKED)
	c.waitClosed()
	waitFor(t, "the session slot to free", func() bool { return dp.sessionCount() == 0 })
	c2, sdp2 := newCaller(t, DataChannelLabel)
	c2.accept(dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: newID(t), SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp2}))
}

// A session whose offer still gathers candidates counts against the limit,
// and the rejected offer creates no session.
func TestOfferSessionLimitCountsSessionsInSetup(t *testing.T) {
	w := newWorld(t)
	dp := New(Options{Dispatcher: w.d, Sandboxes: w.m, Logger: logger(), SessionLimits: SessionLimits{MaxSessions: 1}, IncludeLoopback: true, InterfaceFilter: loopbackOnly})
	dp.Configure([]string{silentSTUN(t)})
	t.Cleanup(dp.Close)
	ch := newChannel(1)
	_, first, answers := w.startOffer(t, dp, ch)
	waitGathering(t, dp, first)

	_, sdp := newCaller(t, DataChannelLabel)
	id := newID(t)
	a := dp.Offer(context.Background(), ch, &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp})
	requireSessionCapacityAnswer(t, a, id)
	if n := dp.sessionCount(); n != 1 {
		t.Fatalf("%d sessions, want only the one in setup", n)
	}
	// Ending the first offer's session frees the slot; its offer answers
	// without waiting for the gathering timeout.
	dp.Close()
	requireEndedAnswer(t, answers, ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE)
}

func TestOfferSessionReservationDuringConstruction(t *testing.T) {
	revoke := func(dp *DataPlane, ch *fakeChannel, id string) {
		dp.Revoke(ch, &ownsandv1.SessionRevocation{Target: &ownsandv1.SessionRevocation_SessionId{SessionId: id}})
	}
	for _, tt := range []struct {
		name   string
		code   ownsandv1.ErrorCode
		cancel func(*DataPlane, *fakeChannel, string)
	}{
		{"constructor failure", ownsandv1.ErrorCode_ERROR_CODE_INTERNAL, nil},
		{"revoked", ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, revoke},
		{"channel lost", ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, func(dp *DataPlane, ch *fakeChannel, _ string) { dp.ChannelLost(ch) }},
		{"channel closed", ownsandv1.ErrorCode_ERROR_CODE_FAILED_PRECONDITION, func(_ *DataPlane, ch *fakeChannel, _ string) { ch.Close() }},
		{"shutdown", ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE, func(dp *DataPlane, _ *fakeChannel, _ string) { dp.Close() }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			dp := New(Options{
				Dispatcher: w.d, Sandboxes: w.m, Logger: logger(), SessionLimits: SessionLimits{MaxSessions: 1},
				IncludeLoopback: true, InterfaceFilter: loopbackOnly,
			})
			t.Cleanup(dp.Close)
			started := make(chan struct{})
			release := make(chan struct{})
			releaseConstructor := sync.OnceFunc(func() { close(release) })
			t.Cleanup(releaseConstructor)
			constructed := make(chan *webrtc.PeerConnection, 1)
			var calls atomic.Int32
			failConstruction := tt.cancel == nil
			shutdown := tt.code == ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE
			dp.newPeerConnection = func(stun []string) (*webrtc.PeerConnection, error) {
				if calls.Add(1) != 1 {
					return dp.createPeerConnection(stun)
				}
				close(started)
				<-release
				if failConstruction {
					return nil, errors.New("test constructor failed")
				}
				pc, err := dp.createPeerConnection(stun)
				constructed <- pc
				return pc, err
			}

			ch := newChannel(1)
			dp.ChannelOpened(ch)
			_, id, answers := w.startOffer(t, dp, ch)
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("peer construction did not start")
			}

			nextChannel := newChannel(2)
			nextCaller, sdp := newCaller(t, DataChannelLabel)
			next := &ownsandv1.SignalingOffer{SessionId: newID(t), SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp}
			dup := &ownsandv1.SignalingOffer{SessionId: id, SandboxId: w.sandbox, ExpiresIn: durationpb.New(time.Hour), Sdp: sdp}
			if failConstruction {
				revoke(dp, ch, next.GetSessionId())
			}
			requireSessionCapacityAnswer(t, dp.Offer(context.Background(), nextChannel, next), next.GetSessionId())
			if a := dp.Offer(context.Background(), ch, dup); a.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT || a.GetSdp() != "" {
				t.Fatalf("duplicate answer %v", a)
			}

			if tt.cancel != nil {
				tt.cancel(dp, ch, id)
			}
			if shutdown {
				if a := dp.Offer(context.Background(), nextChannel, next); a.GetError().GetCode() != ownsandv1.ErrorCode_ERROR_CODE_UNAVAILABLE || a.GetSdp() != "" {
					t.Fatalf("offer after shutdown: %v", a)
				}
			} else {
				// Cancellation must not free the slot while construction owns it.
				requireSessionCapacityAnswer(t, dp.Offer(context.Background(), nextChannel, next), next.GetSessionId())
			}
			if n := calls.Load(); n != 1 {
				t.Errorf("constructor ran %d times while one slot was reserved, want 1", n)
			}
			releaseConstructor()
			requireEndedAnswer(t, answers, tt.code)
			if !failConstruction {
				pc := <-constructed
				if pc == nil || pc.ConnectionState() != webrtc.PeerConnectionStateClosed || pc.RemoteDescription() != nil {
					t.Fatal("canceled construction must close the connection without using it")
				}
			}
			requireNoSession(t, dp)
			dp.mu.Lock()
			pending := len(dp.pendingSessions)
			dp.mu.Unlock()
			if pending != 0 {
				t.Fatalf("pending reservations after setup returned = %d, want 0", pending)
			}
			if shutdown {
				return
			}
			if failConstruction {
				// Capacity refusals leave the revocation for the next attempt.
				requireRefusedOffer(t, dp.Offer(context.Background(), nextChannel, next))
			}
			dp.ChannelOpened(nextChannel)
			nextCaller.accept(dp.Offer(context.Background(), nextChannel, next))
			if n := calls.Load(); n != 2 {
				t.Fatalf("constructor ran %d times after a valid retry, want 2", n)
			}
		})
	}
}
