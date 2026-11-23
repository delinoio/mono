// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// waitWrote waits until the fake writer wrote the frame want, skipping the
// frames that it wrote before (such as the stream open frame). It fails the
// test when the writer does not write want within the timeout.
func waitWrote(t *testing.T, wrote <-chan *ownsandv1.DataFrame, want *ownsandv1.DataFrame, within time.Duration) {
	t.Helper()
	timeout := time.After(within)
	for {
		select {
		case f := <-wrote:
			if f == want {
				return
			}
		case <-timeout:
			t.Fatal("the writer did not write the expected frame")
		}
	}
}

// TestSendMeteredReportsWireBytes checks that a metered frame reports the
// bytes that its send wrote on the connection, and only that frame.
func TestSendMeteredReportsWireBytes(t *testing.T) {
	for _, withCounter := range []bool{true, false} {
		c := NewConn(context.Background(), "device", 1, testLimits)
		var written atomic.Int64
		if withCounter {
			c.SetWireBytes(written.Load)
		}
		// The fake writer writes a 7-byte header and the frame encoding, and
		// publishes each frame it wrote so the test can wait for it.
		wrote := make(chan *ownsandv1.DataFrame, 16)
		send := func(f *ownsandv1.DataFrame) error {
			written.Add(7 + int64(proto.Size(f)))
			wrote <- f
			return nil
		}
		done := runConn(c, blockingRecv(c), send)
		s := openStream(t, c)
		f := acceptedFrame(s)
		got := make(chan int64, 1)
		if err := s.SendMetered(context.Background(), f, func(n int64) { got <- n }); err != nil {
			t.Fatal(err)
		}
		want := 7 + int64(proto.Size(f))
		if !withCounter {
			want = int64(proto.Size(&ownsandv1.ConnectResponse{Frame: f})) + envelopePrefix
		}
		select {
		case n := <-got:
			if n != want {
				t.Fatalf("counter %v: metered %d bytes, want %d", withCounter, n, want)
			}
		case <-time.After(time.Second):
			t.Fatal("the metered frame was not reported")
		}
		// An unmetered frame reports nothing. Wait until the writer wrote it
		// before closing: Close cancels the connection context, and a frame
		// that is still queued could be discarded, which would pass the check
		// below without the frame ever being written.
		unmetered := acceptedFrame(s)
		if err := s.Send(context.Background(), unmetered); err != nil {
			t.Fatal(err)
		}
		waitWrote(t, wrote, unmetered, time.Second)
		c.Close(ErrConnClosed)
		waitRun(t, done, time.Second)
		select {
		case n := <-got:
			t.Fatalf("an unmetered frame reported %d bytes", n)
		default:
		}
	}
}
