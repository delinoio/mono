// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func echoRequest() *ownsandv1.DataFrame {
	return datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{EchoRequested: true})
}

func requireEchoReply(t *testing.T, frame *ownsandv1.DataFrame) {
	t.Helper()
	if reply := frame.GetRelayKeepalive(); frame.GetStreamId() != 0 || reply == nil || reply.GetEchoRequested() {
		t.Fatalf("sent %v, want a stream-zero keepalive with echo disabled", frame)
	}
}

func TestKeepaliveEchoWhileOutboundQueueIsFull(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	clk := newStepClock()
	c := NewConnWithClock(ctx, "device", 1, stepLimits, clk)
	incoming := make(chan *ownsandv1.DataFrame, 5)
	sent := make(chan *ownsandv1.DataFrame, outboundQueue+3)
	writerBlocked := make(chan struct{})
	releaseWriter := make(chan struct{})
	done := runConn(c, func() (*ownsandv1.DataFrame, error) {
		select {
		case f := <-incoming:
			return f, nil
		case <-c.Done():
			return nil, c.Err()
		}
	}, func(f *ownsandv1.DataFrame) error {
		if f.GetSupportedFrameKinds() != nil {
			close(writerBlocked)
			select {
			case <-releaseWriter:
			case <-c.Done():
				return c.Err()
			}
		}
		sent <- f
		return nil
	})
	awaitIdleSendEntry(t, writerBlocked, done)
	closing := openStream(t, c)
	receiving := openStream(t, c)
	for i := 2; i < outboundQueue; i++ {
		f := datapath.MustNewFrame(closing.ID, closing.SandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: int64(i)})
		if err := closing.Send(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	closing.Close(true)
	if got := len(c.out); got != outboundQueue+1 {
		t.Fatalf("queued frame count = %d, want %d with the reserved final", got, outboundQueue+1)
	}

	clk.advance(time.Millisecond)
	for range 4 {
		incoming <- echoRequest()
	}
	incoming <- acceptedFrame(receiving)
	if f, err := receiving.Next(ctx, nil); err != nil || f.GetRequestAccepted() == nil {
		t.Fatalf("Next while the writer was blocked = %v, %v, want request accepted", f, err)
	}
	if got := c.LastReceive(); !got.Equal(clk.Now()) {
		t.Fatalf("LastReceive = %v, want %v", got, clk.Now())
	}

	close(releaseWriter)
	sc := &stepConn{sent: sent, done: done}
	if f := sc.awaitFrame(t); f.GetSupportedFrameKinds() == nil {
		t.Fatalf("sent %v, want supported frame kinds", f)
	}
	requireEchoReply(t, sc.awaitFrame(t))
	for _, s := range []*Stream{closing, receiving} {
		if f := sc.awaitFrame(t); f.GetRelayStreamOpen() == nil || f.GetStreamId() != s.ID {
			t.Fatalf("sent %v, want open frame for stream %d", f, s.ID)
		}
	}
	for i := 2; i < outboundQueue; i++ {
		f := sc.awaitFrame(t)
		if f.GetStreamId() != closing.ID || f.GetRelayWindowUpdate().GetIncrementBytes() != int64(i) {
			t.Fatalf("sent %v, want window update %d for stream %d", f, i, closing.ID)
		}
	}
	if f := sc.awaitFrame(t); f.GetRelayStreamClose() == nil || f.GetStreamId() != closing.ID {
		t.Fatalf("sent %v, want final frame for stream %d", f, closing.ID)
	}
	sc.expectNoFrame(t, "overlapping echo requests shared one reply")
	c.Close(ErrConnClosed)
	waitRun(t, done, time.Second)
}

func TestKeepaliveEchoRestartsOutboundIdleInterval(t *testing.T) {
	sc := runStepConn(t, stepLimits)
	sc.awaitFrame(t)
	sc.clk.advance(stepLimits.Keepalive * 3 / 4)
	sc.deliver(t, echoRequest())
	requireEchoReply(t, sc.awaitFrame(t))
	awaitStepValue(t, "lastSend", &sc.c.lastSend, int64(sc.clk.Elapsed()))

	sc.clk.advance(stepLimits.Keepalive / 2)
	sc.expectNoFrame(t, "the echo restarted the outbound idle interval")
	sc.clk.advance(stepLimits.Keepalive / 2)
	requireEchoReply(t, sc.awaitFrame(t))
}
