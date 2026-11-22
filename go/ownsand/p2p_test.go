// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/pion/webrtc/v4"
)

// holdSendAt makes the session report a full send buffer from the n-th check
// of the buffered amount on (1-based), so a send stops at fragment n-1 and
// waits for the buffer to drain. The returned channel closes at the first
// check that reports the full buffer, which is when the send is waiting.
func holdSendAt(sess *p2pSession, n int64) <-chan struct{} {
	waiting := make(chan struct{})
	var calls atomic.Int64
	sess.sendTurn <- struct{}{}
	defer func() { <-sess.sendTurn }()
	sess.bufferedAmount = func() uint64 {
		call := calls.Add(1)
		if call < n {
			return 0
		}
		if call == n {
			close(waiting)
		}
		return maxBufferedAmount + 1
	}
	return waiting
}

// multiFragmentFrame returns a frame that the SDK splits into three data
// channel messages.
func multiFragmentFrame(t *testing.T, sess *p2pSession, id uint64) *ownsandv1.DataFrame {
	t.Helper()
	f := datapath.MustNewFrame(id, sess.sandboxID, &ownsandv1.WriteFileRequest{
		WriteId: mustID(t),
		Path:    "/torn",
		Data:    bytes.Repeat([]byte("x"), 2*datapath.MaxDataChannelMessageSize+100),
	})
	msgs, err := datapath.EncodeMessages(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) < 3 {
		t.Fatalf("frame has %d messages, want at least 3", len(msgs))
	}
	return f
}

// TestSendCancelledBeforeFirstFragmentKeepsSession checks that a cancelled
// send that wrote nothing leaves the session usable: the next frame is
// decoded by the peer.
func TestSendCancelledBeforeFirstFragmentKeepsSession(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := s.dp.route(ctx)
	if err != nil || sess == nil {
		t.Fatalf("route: %v, %v", sess, err)
	}
	waiting := holdSendAt(sess, 1)

	sendCtx, cancelSend := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- sess.send(sendCtx, multiFragmentFrame(t, sess, 1)) }()
	<-waiting
	cancelSend()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("send: want context.Canceled, got %v", err)
	}
	if sess.isDone() {
		t.Fatal("a send cancelled before its first fragment ended the session")
	}

	holdSendAt(sess, 1<<62)
	want := []byte("after the cancelled send")
	if err := s.WriteFileBytes(ctx, "/after", want, WriteOptions{}); err != nil {
		t.Fatalf("write on the same session: %v", err)
	}
	if got, _ := cp.agent.file("/after"); !bytes.Equal(got, want) {
		t.Fatalf("file %q", got)
	}
	if n := cp.openSessionCount(); n != 1 {
		t.Fatalf("%d sessions opened, want the original session only", n)
	}
}

// TestSendCancelledAfterFirstFragmentRetiresSession checks that a cancelled
// send that already wrote a fragment retires the session: the peer would read
// the next frame as the tail of the torn one. The next operation opens a new
// session on the peer-to-peer path and its frames decode.
func TestSendCancelledAfterFirstFragmentRetiresSession(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportAuto))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	sess, err := s.dp.route(ctx)
	if err != nil || sess == nil {
		t.Fatalf("route: %v, %v", sess, err)
	}
	waiting := holdSendAt(sess, 2)

	sendCtx, cancelSend := context.WithCancel(ctx)
	errc := make(chan error, 1)
	go func() { errc <- sess.send(sendCtx, multiFragmentFrame(t, sess, 1)) }()
	<-waiting
	cancelSend()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Fatalf("send: want context.Canceled, got %v", err)
	}
	if !sess.isDone() {
		t.Fatal("a send cancelled after its first fragment left the session reusable")
	}
	var retired *errSessionRetired
	if !errors.As(sess.endErr, &retired) {
		t.Fatalf("session end error %v, want errSessionRetired", sess.endErr)
	}
	// A concurrent stream of the retired session handles the error like a
	// session end: the handle forgets the session and does not move to the
	// relay.
	if !s.dp.handleSessionError(sess, sess.endErr) {
		t.Fatal("handleSessionError did not treat the retirement as a session end")
	}

	want := bytes.Repeat([]byte("y"), 3*datapath.MaxDataChannelMessageSize)
	if err := s.WriteFileBytes(ctx, "/after", want, WriteOptions{}); err != nil {
		t.Fatalf("write on a new session: %v", err)
	}
	if got, _ := cp.agent.file("/after"); !bytes.Equal(got, want) {
		t.Fatalf("file has %d bytes, want %d", len(got), len(want))
	}
	if n := cp.openSessionCount(); n != 2 {
		t.Fatalf("%d sessions opened, want a second one", n)
	}
	if got := s.Transport(); got != ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_PEER_TO_PEER {
		t.Fatalf("transport %v, want peer-to-peer", got)
	}
}

// TestSendContextDoneWhileWaitingForSendTurnWritesNothing checks that a
// request that is canceled or past its deadline while it waits for the send
// turn writes no message when the send buffer has room, removes its stream and
// keeps the session usable.
func TestSendContextDoneWhileWaitingForSendTurnWritesNothing(t *testing.T) {
	cases := []struct {
		name string
		mk   func(parent context.Context) (context.Context, func(), error)
		want error
	}{
		{"canceled", func(p context.Context) (context.Context, func(), error) {
			c, cancel := context.WithCancel(p)
			return c, cancel, nil
		}, context.Canceled},
		{"deadline", func(p context.Context) (context.Context, func(), error) {
			c, cancel := context.WithTimeout(p, 50*time.Millisecond)
			return c, cancel, nil
		}, context.DeadlineExceeded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cp := newFakeCP(t)
			s := cp.handle(t, cp.client(t, TransportPeerToPeer))
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			sess, err := s.dp.route(ctx)
			if err != nil || sess == nil {
				t.Fatalf("route: %v, %v", sess, err)
			}
			// Hold the send turn like a stalled sender, so the request queues.
			sess.sendTurn <- struct{}{}
			held := true
			releaseTurn := func() {
				if held {
					held = false
					<-sess.sendTurn
				}
			}
			defer releaseTurn()
			reqCtx, stop, _ := tc.mk(ctx)
			defer stop()
			errc := make(chan error, 1)
			go func() {
				_, err := sess.openStream(reqCtx, &ownsandv1.WriteFileRequest{WriteId: mustID(t), Path: "/never", Data: []byte("x")})
				errc <- err
			}()
			// Wait until the stream is registered, then make the context done
			// while the request still waits for the send turn.
			for {
				sess.mu.Lock()
				n := len(sess.streams)
				sess.mu.Unlock()
				if n == 1 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if tc.want == context.Canceled {
				stop()
			}
			<-reqCtx.Done()
			// The request must return while the turn is still held: it
			// observes its own context and does not wait for the sender.
			select {
			case err := <-errc:
				if !errors.Is(err, tc.want) {
					t.Fatalf("openStream: want %v, got %v", tc.want, err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("openStream did not return while the send turn was held")
			}
			releaseTurn()
			if sess.isDone() {
				t.Fatal("the session ended")
			}
			sess.mu.Lock()
			n := len(sess.streams)
			sess.mu.Unlock()
			if n != 0 {
				t.Fatalf("%d streams left", n)
			}
			want := []byte("after")
			if err := s.WriteFileBytes(ctx, "/after", want, WriteOptions{}); err != nil {
				t.Fatalf("write on the same session: %v", err)
			}
			if _, ok := cp.agent.file("/never"); ok {
				t.Fatal("the agent received the canceled request")
			}
			if got, _ := cp.agent.file("/after"); !bytes.Equal(got, want) {
				t.Fatalf("file %q", got)
			}
			if n := cp.openSessionCount(); n != 1 {
				t.Fatalf("%d sessions opened, want 1", n)
			}
		})
	}
}

// TestSendQueuedBehindHeldTurnCancelsIndependently checks that, with the send
// turn held, a canceled request returns at once and sends nothing, while an
// older request that stays queued still sends its frame after the turn is
// released, and that a canceled request does not change the session.
func TestSendQueuedBehindHeldTurnCancelsIndependently(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := s.dp.route(ctx)
	if err != nil || sess == nil {
		t.Fatalf("route: %v, %v", sess, err)
	}
	sess.sendTurn <- struct{}{}
	held := true
	releaseTurn := func() {
		if held {
			held = false
			<-sess.sendTurn
		}
	}
	defer releaseTurn()

	write := func(path string) *ownsandv1.WriteFileRequest {
		return &ownsandv1.WriteFileRequest{
			WriteId: mustID(t), Path: path, Data: []byte("x"),
			Finish: &ownsandv1.WriteFileFinish{SizeBytes: 1, Mode: 0o644},
		}
	}
	streamCount := func() int {
		sess.mu.Lock()
		defer sess.mu.Unlock()
		return len(sess.streams)
	}
	waitStreams := func(n int) {
		for deadline := time.Now().Add(15 * time.Second); streamCount() != n; time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatalf("%d streams registered, want %d", streamCount(), n)
			}
		}
	}

	keepCtx, keepCancel := context.WithCancel(ctx)
	defer keepCancel()
	keepErr := make(chan error, 1)
	var kept *p2pStream
	go func() {
		st, err := sess.openStream(keepCtx, write("/kept"))
		kept = st
		keepErr <- err
	}()
	waitStreams(1)
	dropCtx, dropCancel := context.WithCancel(ctx)
	dropErr := make(chan error, 1)
	go func() {
		_, err := sess.openStream(dropCtx, write("/dropped"))
		dropErr <- err
	}()
	waitStreams(2)

	dropCancel()
	select {
	case err := <-dropErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled request: want context.Canceled, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the canceled request did not return while the send turn was held")
	}
	waitStreams(1)
	select {
	case err := <-keepErr:
		t.Fatalf("the queued request returned while the turn was held: %v", err)
	default:
	}

	releaseTurn()
	select {
	case err := <-keepErr:
		if err != nil {
			t.Fatalf("queued request: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the queued request did not send after the turn was released")
	}
	defer kept.close()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(time.Millisecond) {
		if _, ok := cp.agent.file("/kept"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent did not receive the queued request")
		}
	}
	if _, ok := cp.agent.file("/dropped"); ok {
		t.Fatal("the agent received the canceled request")
	}
	if sess.isDone() {
		t.Fatal("the session ended")
	}
	if err := s.WriteFileBytes(ctx, "/after", []byte("after"), WriteOptions{}); err != nil {
		t.Fatalf("write on the same session: %v", err)
	}
	if n := cp.openSessionCount(); n != 1 {
		t.Fatalf("%d sessions opened, want 1", n)
	}
}

// TestSendWaitingForSendTurnObservesSessionEnd checks that a request that waits
// for the send turn returns the session's end error when the session ends.
func TestSendWaitingForSendTurnObservesSessionEnd(t *testing.T) {
	cp := newFakeCP(t)
	s := cp.handle(t, cp.client(t, TransportPeerToPeer))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sess, err := s.dp.route(ctx)
	if err != nil || sess == nil {
		t.Fatalf("route: %v, %v", sess, err)
	}
	sess.sendTurn <- struct{}{}
	defer func() { <-sess.sendTurn }()
	errc := make(chan error, 1)
	go func() {
		_, err := sess.openStream(ctx, &ownsandv1.WriteFileRequest{WriteId: mustID(t), Path: "/never", Data: []byte("x")})
		errc <- err
	}()
	for deadline := time.Now().Add(15 * time.Second); ; time.Sleep(time.Millisecond) {
		sess.mu.Lock()
		n := len(sess.streams)
		sess.mu.Unlock()
		if n == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the request did not register its stream")
		}
	}
	sess.close()
	select {
	case err := <-errc:
		var failed *errP2PFailed
		if !errors.As(err, &failed) {
			t.Fatalf("want the session end error, got %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the request did not return when the session ended")
	}
}

// newDispatchSession returns a session with open streams 1 and 2 for tests
// that call dispatch directly.
func newDispatchSession(t *testing.T) (*p2pSession, *p2pStream, *p2pStream) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	s := &p2pSession{
		pc:           pc,
		streams:      map[uint64]*p2pStream{},
		done:         make(chan struct{}),
		queueTimeout: time.Second,
	}
	mk := func(id uint64) *p2pStream {
		st := &p2pStream{s: s, id: id, ch: make(chan *ownsandv1.DataFrame, 4), closed: make(chan struct{}), failed: make(chan struct{})}
		s.streams[id] = st
		return st
	}
	return s, mk(1), mk(2)
}

func TestDispatchInvalidSessionEndDoesNotEndSession(t *testing.T) {
	s, st1, st2 := newDispatchSession(t)

	// A session-end kind without its payload fails only its open stream.
	s.dispatch(&ownsandv1.DataFrame{StreamId: 1, Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END})
	select {
	case <-st1.failed:
		var inv *errInvalidFrame
		if !errors.As(st1.failErr, &inv) {
			t.Fatalf("stream 1 error = %v, want errInvalidFrame", st1.failErr)
		}
	default:
		t.Fatal("stream 1 was not failed")
	}
	if s.isDone() {
		t.Fatal("an invalid session-end frame ended the session")
	}

	// A session-end kind on stream 0 and on an unopened stream is ignored.
	s.dispatch(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END})
	s.dispatch(&ownsandv1.DataFrame{StreamId: 9, Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_SESSION_END})
	if s.isDone() {
		t.Fatal("an ignored invalid session-end frame ended the session")
	}

	// Another stream keeps receiving frames.
	f := datapath.MustNewFrame(2, "", &ownsandv1.ErrorDetail{Message: "x"})
	s.dispatch(f)
	select {
	case got := <-st2.ch:
		if got != f {
			t.Fatal("stream 2 received another frame")
		}
	default:
		t.Fatal("stream 2 did not receive its frame")
	}
}

func TestDispatchValidSessionEndEndsSession(t *testing.T) {
	s, _, _ := newDispatchSession(t)
	s.dispatch(datapath.MustNewFrame(1, "", &ownsandv1.SessionEnd{Reason: ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED}))
	if !s.isDone() {
		t.Fatal("a valid session-end frame did not end the session")
	}
	var ended *errSessionEnded
	if !errors.As(s.endErr, &ended) || ended.reason != ownsandv1.SessionEndReason_SESSION_END_REASON_EXPIRED {
		t.Fatalf("end error = %v, want errSessionEnded with the reason", s.endErr)
	}
}
