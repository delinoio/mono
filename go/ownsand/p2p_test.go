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
)

// holdSendAt makes the session report a full send buffer from the n-th check
// of the buffered amount on (1-based), so a send stops at fragment n-1 and
// waits for the buffer to drain. The returned channel closes at the first
// check that reports the full buffer, which is when the send is waiting.
func holdSendAt(sess *p2pSession, n int64) <-chan struct{} {
	waiting := make(chan struct{})
	var calls atomic.Int64
	sess.sendMu.Lock()
	defer sess.sendMu.Unlock()
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
			sess.sendMu.Lock()
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
			sess.sendMu.Unlock()
			if err := <-errc; !errors.Is(err, tc.want) {
				t.Fatalf("openStream: want %v, got %v", tc.want, err)
			}
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
