// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// raceKeepalive is an ordinary frame that consumes no credit.
func raceKeepalive() *ownsandv1.DataFrame {
	return datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
}

// raceCreditFrame is a credit-consuming frame, as the agent sends it.
func raceCreditFrame(s *Stream) *ownsandv1.DataFrame {
	return datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.ExecEvent{
		Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: []byte("x")}}})
}

// raceFreeOneToken plays the writer for one frame: it takes the oldest queued
// frame and returns its ordinary token, so that one blocked ordinary enqueue
// can proceed.
func raceFreeOneToken(t *testing.T, c *Conn) {
	t.Helper()
	select {
	case of := <-c.out:
		c.dequeued(of)
	default:
		t.Fatal("the outbound queue is empty")
	}
}

// raceWaitForToken waits for enqueueFrame to evaluate the caller's Done
// channel in the token select, after its first check for stream closure.
func raceWaitForToken(t *testing.T, reached <-chan struct{}) {
	t.Helper()
	select {
	case <-reached:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueueFrame did not reach the outbound token wait")
	}
}

func startQueueTestWorker(t *testing.T, name string, work func(context.Context)) (reached, done <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	probe := &doneProbeContext{Context: ctx, reached: make(chan struct{})}
	finished := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		if !waitForRelayTestWorker(finished, relayWorkerCleanupTimeout) {
			t.Errorf("%s worker was still running at the cleanup deadline", name)
		}
	})
	go func() {
		defer close(finished)
		work(probe)
	}()
	return probe.reached, finished
}

// raceDrain starts the writer, waits until it sent n frames and returns the
// recorder. The caller closes the connection with the returned function.
func raceDrain(t *testing.T, c *Conn, n int) (*sentFrames, func()) {
	t.Helper()
	rec := newSentFrames()
	done := runConn(c, blockingRecv(c), rec.send)
	stop := func() {
		c.Close(ErrConnClosed)
		waitRun(t, done, 2*time.Second)
	}
	for i := 0; i < n; i++ {
		select {
		case <-rec.notify:
		case <-time.After(5 * time.Second):
			stop()
			t.Fatalf("writer sent %d of %d frames", i, n)
		}
	}
	return rec, stop
}

func raceStreamFrames(frames []*ownsandv1.DataFrame, id uint64) []*ownsandv1.DataFrame {
	var out []*ownsandv1.DataFrame
	for _, f := range frames {
		if f.GetStreamId() == id {
			out = append(out, f)
		}
	}
	return out
}

// TestLateSendAfterCloseIsRejected holds a sender that waits for an outbound
// token, closes its stream, and then frees a token. The sender must not queue
// its frame behind the final frame: it fails with ErrStreamClosed, returns its
// token, and its write callback never runs.
func TestLateSendAfterCloseIsRejected(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	filler := fillOutbound(c)

	var written atomic.Int64
	errCh := make(chan error, 1)
	reached, _ := startQueueTestWorker(t, "SendMetered", func(ctx context.Context) {
		errCh <- s.SendMetered(ctx, raceKeepalive(), func(int64) { written.Add(1) })
	})
	raceWaitForToken(t, reached)

	closeReturnsPromptly(t, s, true)
	raceFreeOneToken(t, c)
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrStreamClosed) {
			t.Fatalf("late send = %v, want ErrStreamClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the late send did not return after a token was freed")
	}
	if got := len(c.slots); got != outboundQueue-1 {
		t.Fatalf("tokens in use = %d, want %d (the rejected frame returned its token)", got, outboundQueue-1)
	}
	if got := len(c.out); got != filler+1 {
		t.Fatalf("queued frames = %d, want %d (the fillers and the final frame only)", got, filler+1)
	}

	// The writer sends the fillers, the final frame and its own supported
	// kinds frame; the stream open frame was taken by raceFreeOneToken.
	rec, stop := raceDrain(t, c, filler+2)
	defer stop()
	got := raceStreamFrames(rec.snapshot(), s.ID)
	if len(got) != 1 || got[0].GetRelayStreamClose() == nil {
		t.Fatalf("frames of the closed stream = %d, want only its close frame", len(got))
	}
	if n := written.Load(); n != 0 {
		t.Fatalf("write callback ran %d times for a rejected frame", n)
	}
}

// TestSendOnClosedStreamFailsWithoutWaiting checks that a stream that already
// closed rejects ordinary frames at once, even when the queue has no free
// token, and takes no token.
func TestSendOnClosedStreamFailsWithoutWaiting(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fillOutbound(c)
	closeReturnsPromptly(t, s, true)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	var written atomic.Int64
	if err := s.SendMetered(ctx, raceKeepalive(), func(int64) { written.Add(1) }); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("send on a closed stream = %v, want ErrStreamClosed", err)
	}
	if err := s.Send(ctx, raceKeepalive()); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("Send on a closed stream = %v, want ErrStreamClosed", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("a send on a closed stream waited for the queue")
	}
	if got := len(c.slots); got != outboundQueue {
		t.Fatalf("tokens in use = %d, want %d (unchanged)", got, outboundQueue)
	}
	if written.Load() != 0 {
		t.Fatal("write callback ran for a rejected frame")
	}
}

// TestLateWindowUpdateAfterCloseIsRejected is the Consumed variant: a credit
// return that waits for a token when its stream closes is dropped.
func TestLateWindowUpdateAfterCloseIsRejected(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fc := raceCreditFrame(s)
	if err := s.recv.Receive(datapath.FrameCost(fc)); err != nil {
		t.Fatal(err)
	}
	filler := fillOutbound(c)

	reached, done := startQueueTestWorker(t, "Consumed", func(ctx context.Context) {
		s.Consumed(ctx, fc)
	})
	raceWaitForToken(t, reached)

	closeReturnsPromptly(t, s, true)
	raceFreeOneToken(t, c)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Consumed did not return after a token was freed")
	}
	if got := len(c.slots); got != outboundQueue-1 {
		t.Fatalf("tokens in use = %d, want %d", got, outboundQueue-1)
	}

	rec, stop := raceDrain(t, c, filler+2)
	defer stop()
	got := raceStreamFrames(rec.snapshot(), s.ID)
	if len(got) != 1 || got[0].GetRelayStreamClose() == nil {
		t.Fatalf("frames of the closed stream = %d, want only its close frame", len(got))
	}
}

func TestFullQueueCloseFixtureJoinsQueueWorkers(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"SendMetered", "Consumed"} {
		t.Run(name, func(t *testing.T) {
			c := NewConn(t.Context(), "device", 1, closeTestLimits)
			s, err := c.Open(t.Context(), "grant-a", "sandbox")
			if err != nil {
				t.Fatal(err)
			}
			fillOutbound(c)

			var done <-chan struct{}
			errCh := make(chan error, 1)
			if !t.Run("exit with a full queue", func(t *testing.T) {
				var reached <-chan struct{}
				if name == "SendMetered" {
					reached, done = startQueueTestWorker(t, name, func(ctx context.Context) {
						errCh <- s.SendMetered(ctx, raceKeepalive(), nil)
					})
				} else {
					fc := raceCreditFrame(s)
					if err := s.recv.Receive(datapath.FrameCost(fc)); err != nil {
						t.Fatal(err)
					}
					reached, done = startQueueTestWorker(t, name, func(ctx context.Context) {
						s.Consumed(ctx, fc)
					})
				}
				raceWaitForToken(t, reached)
			}) {
				return
			}

			select {
			case <-done:
			default:
				t.Fatal("fixture cleanup returned before the queue worker finished")
			}
			if name == "SendMetered" {
				if err := <-errCh; !errors.Is(err, context.Canceled) {
					t.Errorf("SendMetered after fixture cleanup = %v, want context canceled", err)
				}
			}
			if got := len(c.slots); got != outboundQueue {
				t.Errorf("tokens in use after cleanup = %d, want %d", got, outboundQueue)
			}
		})
	}
}

// TestQueuedFramesStayAheadOfTheFinalFrame queues an ordinary frame and a
// window update, closes the stream, and checks the writer sends them in that
// order before the close frame, which is the only frame after the close.
func TestQueuedFramesStayAheadOfTheFinalFrame(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	s, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Send(context.Background(), raceKeepalive()); err != nil {
		t.Fatal(err)
	}
	fc := raceCreditFrame(s)
	if err := s.recv.Receive(datapath.FrameCost(fc)); err != nil {
		t.Fatal(err)
	}
	s.Consumed(context.Background(), fc)
	closeReturnsPromptly(t, s, true)
	if err := s.Send(context.Background(), raceKeepalive()); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("send after close = %v, want ErrStreamClosed", err)
	}

	// open, keepalive, window update, close, and the supported kinds frame.
	rec, stop := raceDrain(t, c, 5)
	defer stop()
	got := raceStreamFrames(rec.snapshot(), s.ID)
	if len(got) != 4 {
		t.Fatalf("frames of the stream = %d, want 4", len(got))
	}
	if got[0].GetRelayStreamOpen() == nil || got[1].GetRelayKeepalive() == nil ||
		got[2].GetRelayWindowUpdate() == nil || got[3].GetRelayStreamClose() == nil {
		t.Fatalf("frame order = %v %v %v %v, want open, keepalive, window update, close",
			datapath.PayloadKind(got[0]), datapath.PayloadKind(got[1]), datapath.PayloadKind(got[2]), datapath.PayloadKind(got[3]))
	}
}

// TestFullQueueCloseNeverBlocksAndOtherStreamsEnqueue closes a stream while
// the ordinary queue is full: the close returns at once, a rejected late frame
// of that stream takes no place, and another stream still enqueues once a
// token is free.
func TestFullQueueCloseNeverBlocksAndOtherStreamsEnqueue(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, closeTestLimits)
	a, err := c.Open(context.Background(), "grant-a", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Open(context.Background(), "grant-b", "sandbox")
	if err != nil {
		t.Fatal(err)
	}
	fillOutbound(c)
	closeReturnsPromptly(t, a, true)
	if got := len(c.out); got != outboundQueue+1 {
		t.Fatalf("queued frames = %d, want %d (the ordinary queue plus the final frame)", got, outboundQueue+1)
	}
	lateCtx, cancelLate := context.WithTimeout(context.Background(), time.Second)
	defer cancelLate()
	if err := a.Send(lateCtx, raceKeepalive()); !errors.Is(err, ErrStreamClosed) {
		t.Fatalf("late send = %v, want ErrStreamClosed", err)
	}
	if got := len(c.out); got != outboundQueue+1 {
		t.Fatalf("queued frames after the late send = %d, want %d", got, outboundQueue+1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := b.Send(ctx, raceKeepalive()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send of the other stream with a full queue = %v, want deadline exceeded", err)
	}
	raceFreeOneToken(t, c)
	if err := b.Send(context.Background(), raceKeepalive()); err != nil {
		t.Fatalf("send of the other stream with a free token = %v", err)
	}
	if got := len(c.slots); got != outboundQueue {
		t.Fatalf("tokens in use = %d, want %d", got, outboundQueue)
	}
}
