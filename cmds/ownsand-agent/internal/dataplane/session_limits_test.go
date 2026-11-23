// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build linux || darwin

package dataplane

import (
	"context"
	"sync"
	"testing"
	"time"

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
