// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// recvTestContext lets a test coordinate cancellation with a receive check.
type recvTestContext struct {
	context.Context
	beforeDone func()
	beforeErr  func()
}

func (c *recvTestContext) Done() <-chan struct{} {
	if c.beforeDone != nil {
		c.beforeDone()
	}
	return c.Context.Done()
}

func (c *recvTestContext) Err() error {
	if c.beforeErr != nil {
		c.beforeErr()
	}
	return c.Context.Err()
}

func TestStreamRecvContextWinsAtHandoff(t *testing.T) {
	tests := []struct {
		name  string
		ready func(s *p2pSession, st *p2pStream)
	}{
		{name: "queued frame", ready: func(_ *p2pSession, st *p2pStream) {
			st.ch <- datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "queued"})
		}},
		{name: "stream failure", ready: func(_ *p2pSession, st *p2pStream) {
			st.fail(errors.New("stream failed"))
		}},
		{name: "session end", ready: func(s *p2pSession, _ *p2pStream) {
			s.endErr = errors.New("session ended")
			close(s.done)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, st, _ := newDispatchSession(t)
			tt.ready(s, st)
			ctx := newExpiringContext()
			entered := false
			gated := &recvTestContext{Context: ctx, beforeErr: func() {
				if entered {
					ctx.expire()
				}
				entered = true
			}}
			if f, err := st.recv(gated); f != nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("recv = (%v, %v), want (nil, context.DeadlineExceeded)", f, err)
			}
		})
	}
}

func TestStreamRecvCancelWithReadyFrame(t *testing.T) {
	tests := []struct {
		name string
		end  func(st *p2pStream)
	}{
		{name: "active stream", end: func(*p2pStream) {}},
		{name: "failed stream", end: func(st *p2pStream) { st.fail(errors.New("stream failed")) }},
		{name: "ended session", end: func(st *p2pStream) {
			st.s.endErr = errors.New("session ended")
			close(st.s.done)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Done runs after the fast queue check. Repeat so select can take
			// the frame directly or drain it after a failure or session end.
			for i := range 32 {
				_, st, _ := newDispatchSession(t)
				ctx, cancel := context.WithCancel(t.Context())
				gated := &recvTestContext{Context: ctx, beforeDone: func() {
					st.ch <- datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "queued"})
					tt.end(st)
					cancel()
				}}
				f, err := st.recv(gated)
				cancel()
				if f != nil || !errors.Is(err, context.Canceled) {
					t.Fatalf("iteration %d: recv = (%v, %v), want (nil, context.Canceled)", i, f, err)
				}
			}
		})
	}
}

func TestStreamRecvRejectsLatePeerEvents(t *testing.T) {
	tests := []struct {
		name    string
		payload proto.Message
	}{
		{name: "exec output", payload: &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{
			Stdout: &ownsandv1.ExecOutput{Data: []byte("output")},
		}}},
		{name: "exec exit", payload: exitedEvent(0)},
		{name: "file chunk", payload: &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_Chunk{
			Chunk: &ownsandv1.FileChunk{Data: []byte("data")},
		}}},
		{name: "file end", payload: &ownsandv1.FileReadEvent{Event: &ownsandv1.FileReadEvent_End{
			End: &ownsandv1.FileReadEnd{},
		}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
			defer stop()
			cp := newFakeCP(t)
			waiting := make(chan struct{})
			cp.p2pScript = func(int) p2pAction {
				return p2pAction{raw: func(id uint64, sandboxID string) *ownsandv1.DataFrame {
					select {
					case <-waiting:
					case <-ctx.Done():
					}
					return datapath.MustNewFrame(id, sandboxID, tt.payload)
				}}
			}
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			sess, err := s.dp.route(ctx)
			if err != nil || sess == nil {
				t.Fatalf("open peer session: %v", err)
			}
			callCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			waitingToSend := true
			atReceive := false
			gated := &recvTestContext{Context: callCtx, beforeDone: func() {
				// The send turn also checks Done. Let the request go out first.
				if waitingToSend {
					waitingToSend = false
					return
				}
				close(waiting)
				requireReady(t, ctx, "peer frame queued", func() bool {
					sess.mu.Lock()
					defer sess.mu.Unlock()
					st := sess.streams[1]
					return st != nil && len(st.ch) > 0
				}, nil)
				atReceive = true
			}, beforeErr: func() {
				// Let select take the frame, then cancel at the handoff check.
				if atReceive {
					cancel()
				}
			}}

			switch tt.payload.(type) {
			case *ownsandv1.ExecEvent:
				calls := 0
				result, err := s.Exec(gated, ExecParams{Command: []string{"run"}, OnEvent: func(ExecEvent) error {
					calls++
					return nil
				}})
				if result != nil || calls != 0 || !errors.Is(err, context.Canceled) {
					t.Fatalf("Exec = (%+v, %v), callbacks = %d; want context.Canceled and no result or callback", result, err, calls)
				}
			case *ownsandv1.FileReadEvent:
				var output bytes.Buffer
				n, err := s.ReadFile(gated, "/file", &output)
				if n != 0 || output.Len() != 0 || !errors.Is(err, context.Canceled) {
					t.Fatalf("ReadFile = (%d, %v), output = %q; want context.Canceled and no output", n, err, output.Bytes())
				}
			}
		})
	}
}

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
