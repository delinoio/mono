// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
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
			handleDone := make(chan struct{})
			want := []*ownsandv1.DataFrame{
				datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "one"}),
				datapath.MustNewFrame(st.id, "", &ownsandv1.ErrorDetail{Message: "two"}),
			}
			for _, f := range want {
				st.ch <- f
			}
			tt.setup(s, st)
			for i, w := range want {
				f, err := st.recvUntilHandleClose(t.Context(), handleDone)
				if err != nil || f != w {
					t.Fatalf("recv %d = (%v, %v), want the queued frame", i, f, err)
				}
			}
			if f, err := st.recvUntilHandleClose(t.Context(), handleDone); f != nil || !errors.Is(err, tt.wantErr) {
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
	const first = "first\n"
	var streamID atomic.Uint64
	cp.p2pScript = func(int) p2pAction {
		return p2pAction{raw: func(id uint64, sandboxID string) *ownsandv1.DataFrame {
			streamID.Store(id)
			return datapath.MustNewFrame(id, sandboxID, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{
				Stdout: &ownsandv1.ExecOutput{Data: []byte(first)},
			}})
		}}
	}
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	execCtx, cancelExec := context.WithCancel(ctx)
	defer cancelExec()

	calls := 0
	result, err := s.Exec(execCtx, ExecParams{Command: []string{"run"}, OnEvent: func(ev ExecEvent) error {
		calls++
		if calls > 1 {
			return nil
		}
		if ev.Kind != ExecEventStdout || ev.Offset != 0 || string(ev.Data) != first {
			t.Fatalf("fixture: first event = (%v, %d, %q), want stdout at offset 0 with %q", ev.Kind, ev.Offset, ev.Data, first)
		}
		sess := s.installedSession()
		if sess == nil {
			t.Fatal("fixture: missing peer session for the active exec stream")
		}
		id := streamID.Load()
		sess.mu.Lock()
		st := sess.streams[id]
		sess.mu.Unlock()
		if st == nil {
			t.Fatalf("fixture: missing active exec stream %d", id)
		}
		offset := ev.Offset + int64(len(ev.Data))
		followUp := datapath.MustNewFrame(id, s.id, &ownsandv1.ExecEvent{Event: &ownsandv1.ExecEvent_Stdout{
			Stdout: &ownsandv1.ExecOutput{Offset: offset, Data: []byte("later\n")},
		}})
		// Exec waits for this callback before reading the stream again.
		// Check the queued frame, then put it back for the next receive.
		sess.dispatch(followUp)
		select {
		case queued := <-st.ch:
			if queued != followUp {
				t.Fatalf("fixture: exec stream %d has the wrong frame, want follow-up stdout at offset %d", id, offset)
			}
			st.ch <- queued
		default:
			t.Fatalf("fixture: exec stream %d is missing queued follow-up stdout at offset %d", id, offset)
		}
		cancelExec()
		return nil
	}})
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("Exec = (%+v, %v), want (nil, context.Canceled)", result, err)
	}
	if calls != 1 {
		t.Fatalf("Exec callbacks = %d, want exactly one before cancellation", calls)
	}
}

func TestPeerCloseDuringOperationWakesDetachedReceive(t *testing.T) {
	_, st, _ := newDispatchSession(t)
	receiving := make(chan struct{})
	d := newDataPath(nil)
	ctx := &recvTestContext{Context: t.Context(), beforeDone: func() { close(receiving) }}
	done := make(chan error, 1)
	go func() {
		_, err := st.recvUntilHandleClose(ctx, d.done)
		done <- err
	}()
	select {
	case <-receiving:
	case <-time.After(10 * time.Second):
		t.Fatal("stream did not start waiting for a frame")
	}
	// This stream is outside the handle's current session. Its own end
	// signal stays open when the handle closes.
	d.close()
	requireHandleClosed(t, waitOpErr(t, done))
}

func TestPeerCloseQueuedReceiveChecksDeliveryBoundary(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		_, st, _ := newDispatchSession(t)
		st.ch <- datapath.MustNewFrame(st.id, "", exitedEvent(0))
		d := newDataPath(nil)
		ctx, cancel := context.WithCancel(t.Context())
		t.Cleanup(cancel)
		checks := 0
		gated := &recvTestContext{Context: ctx, beforeErr: func() {
			checks++
			if checks == 2 {
				d.close()
				if cancelCaller {
					cancel()
				}
			}
		}}
		want := errHandleClosed
		if cancelCaller {
			want = context.Canceled
		}
		if f, err := st.recvUntilHandleClose(gated, d.done); f != nil || err != want {
			t.Fatalf("recv with caller canceled = %v: got (%v, %v), want (nil, %v)", cancelCaller, f, err, want)
		}
	}
}

func TestPeerExecCloseWithQueuedOutputAndExit(t *testing.T) {
	for _, tr := range []Transport{TransportAuto, TransportPeerToPeer} {
		for _, cc := range closeCases {
			for _, attach := range []bool{false, true} {
				op := "Exec"
				if attach {
					op = "AttachExec"
				}
				t.Run(tr.String()+"/"+cc.name+"/"+op, func(t *testing.T) {
					cp := newFakeCP(t)
					cp.p2pScript = func(int) p2pAction { return p2pAction{omitKeepalives: true} }
					s := cp.handle(t, cp.client(t, tr))
					queue := make(chan struct{})
					cp.agent.run = func(_ []string, e *fakeExec) {
						e.write(0, []byte("first\n"))
						select {
						case <-queue:
							e.write(0, []byte("queued\n"))
							e.finish(exitedEvent(0))
						case <-t.Context().Done():
						}
					}
					const id = "01900000-0000-7000-8000-000000000001"
					if attach {
						cp.agent.startExec(cp, &ownsandv1.ExecRequest{ExecId: id, Command: []string{"run"}})
					}
					entered, release := make(chan struct{}), make(chan struct{})
					defer close(release)
					out := make(chan execResult, 1)
					var stdout []string
					onEvent := func(ev ExecEvent) error {
						if ev.Kind == ExecEventStdout {
							stdout = append(stdout, string(ev.Data))
							if len(stdout) == 1 {
								close(entered)
								<-release
							}
						}
						return nil
					}
					go func() {
						var res *ExecResult
						var err error
						if attach {
							res, err = s.AttachExec(t.Context(), id, AttachParams{OnEvent: onEvent})
						} else {
							res, err = s.Exec(t.Context(), ExecParams{ExecID: id, Command: []string{"run"}, OnEvent: onEvent})
						}
						out <- execResult{res: res, err: err}
					}()
					select {
					case <-entered:
					case r := <-out:
						t.Fatalf("operation ended before stdout: %+v", r)
					case <-time.After(10 * time.Second):
						t.Fatal("first stdout did not reach the callback")
					}
					sess := s.installedSession()
					close(queue)
					waitFor(t, "queued stdout and exit", func() bool {
						sess.mu.Lock()
						defer sess.mu.Unlock()
						for _, st := range sess.streams {
							if len(st.ch) == 2 {
								return true
							}
						}
						return false
					})
					cc.close(t, s)
					release <- struct{}{}
					r := waitExec(t, out)
					requireHandleClosed(t, r.err)
					if r.res != nil || len(stdout) != 1 || stdout[0] != "first\n" {
						t.Fatalf("result = %+v, stdout = %q", r.res, stdout)
					}
					if s.Transport() != p2pTransport || len(cp.authRequests()) != 0 || cp.agent.totalRuns() != 1 {
						t.Fatalf("transport = %v, AuthorizeRelay calls = %d, runs = %d", s.Transport(), len(cp.authRequests()), cp.agent.totalRuns())
					}
				})
			}
		}
	}
}
