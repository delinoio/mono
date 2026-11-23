// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// TestStreamRecvCanceledContextSkipsQueuedFrames checks that a caller whose
// context is done receives its context error and no queued frame, even when
// the queue still holds frames and when the stream has failed or the session
// has ended.
func TestStreamRecvCanceledContextSkipsQueuedFrames(t *testing.T) {
	queued := func(st *p2pStream) {
		st.ch <- datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "queued"})
	}
	tests := []struct {
		name  string
		setup func(s *p2pSession, st *p2pStream)
	}{
		{name: "active stream", setup: func(s *p2pSession, st *p2pStream) {}},
		{name: "failed stream", setup: func(s *p2pSession, st *p2pStream) { st.fail(errors.New("stream failed")) }},
		{name: "ended session", setup: func(s *p2pSession, st *p2pStream) {
			s.endErr = errors.New("session ended")
			close(s.done)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, st, _ := newDispatchSession(t)
			queued(st)
			queued(st)
			tt.setup(s, st)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			f, err := st.recv(ctx)
			if f != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("recv = (%v, %v), want (nil, context.Canceled)", f, err)
			}
			if got := len(st.ch); got != 2 {
				t.Fatalf("recv consumed a queued frame: %d left, want 2", got)
			}
		})
	}
}

// TestStreamRecvDrainsQueuedFramesForActiveContext checks that a caller with a
// live context still receives the frames that were queued before a stream
// failure or the end of the session, and then the failure or end error.
func TestStreamRecvDrainsQueuedFramesForActiveContext(t *testing.T) {
	failErr := errors.New("stream failed")
	endErr := errors.New("session ended")
	tests := []struct {
		name    string
		setup   func(s *p2pSession, st *p2pStream)
		wantErr error
	}{
		{name: "failed stream", wantErr: failErr, setup: func(s *p2pSession, st *p2pStream) { st.fail(failErr) }},
		{name: "ended session", wantErr: endErr, setup: func(s *p2pSession, st *p2pStream) {
			s.endErr = endErr
			close(s.done)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, st, _ := newDispatchSession(t)
			want := []*ownsandv1.DataFrame{
				datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "one"}),
				datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "two"}),
			}
			for _, f := range want {
				st.ch <- f
			}
			tt.setup(s, st)
			for i, w := range want {
				f, err := st.recv(context.Background())
				if err != nil || f != w {
					t.Fatalf("recv %d = (%v, %v), want the queued frame", i, f, err)
				}
			}
			if f, err := st.recv(context.Background()); f != nil || !errors.Is(err, tt.wantErr) {
				t.Fatalf("recv after drain = (%v, %v), want (nil, %v)", f, err, tt.wantErr)
			}
		})
	}
}

// TestStreamRecvCancelStopsDrainingRefilledQueue checks that a consumer that
// reads a queue which a producer keeps full stops at the next receive after
// its context is canceled, instead of reading on while frames are available.
func TestStreamRecvCancelStopsDrainingRefilledQueue(t *testing.T) {
	_, st, _ := newDispatchSession(t)
	refill := func() {
		for len(st.ch) < cap(st.ch) {
			st.ch <- datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "x"})
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	refill()
	received := 0
	for {
		f, err := st.recv(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("recv error = %v, want context.Canceled", err)
			}
			break
		}
		if f == nil {
			t.Fatal("recv returned a nil frame without an error")
		}
		received++
		refill()
		if received == 3 {
			cancel()
		}
		if received > 3 {
			t.Fatalf("recv returned frame %d after the cancellation", received)
		}
	}
	if received != 3 {
		t.Fatalf("received %d frames, want exactly 3 (none after the cancellation)", received)
	}
}

// TestExecPeerStopsDeliveringQueuedEventsAfterCancel checks that a peer exec
// whose caller cancels inside the event callback gets no further callback for
// the events that are already queued on its stream.
func TestExecPeerStopsDeliveringQueuedEventsAfterCancel(t *testing.T) {
	cp := newFakeCP(t)
	// The agent finishes writing before the callback returns, so the stream
	// queue holds the remaining events when the callback cancels.
	written := make(chan struct{})
	cp.agent.run = func(_ []string, e *fakeExec) {
		e.write(0, []byte("first\n"))
		<-written
		for i := 0; i < 8; i++ {
			e.write(0, []byte("later\n"))
		}
		e.finish(exitedEvent(0))
	}
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	var canceled atomic.Bool
	var after atomic.Int64
	_, err := s.Exec(execCtx, ExecParams{Command: []string{"run"}, OnEvent: func(ev ExecEvent) error {
		if canceled.Load() {
			after.Add(1)
			return nil
		}
		if ev.Kind != ExecEventStdout {
			return nil
		}
		canceled.Store(true)
		cancelExec()
		close(written)
		// Wait until the peer's next event is queued on the stream, so that
		// the receive after this callback finds a queued frame.
		sess := s.installedSession()
		for {
			sess.mu.Lock()
			queued := 0
			for _, st := range sess.streams {
				queued += len(st.ch)
			}
			sess.mu.Unlock()
			if queued > 0 || ctx.Err() != nil {
				return nil
			}
			runtime.Gosched()
		}
	}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("exec error = %v, want context.Canceled", err)
	}
	if n := after.Load(); n != 0 {
		t.Fatalf("%d callbacks after the cancellation, want 0", n)
	}
}
