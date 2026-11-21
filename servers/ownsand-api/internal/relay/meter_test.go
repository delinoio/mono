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

// TestSendMeteredReportsWireBytes checks that a metered frame reports the
// bytes that its send wrote on the connection, and only that frame.
func TestSendMeteredReportsWireBytes(t *testing.T) {
	for _, withCounter := range []bool{true, false} {
		c := NewConn(context.Background(), "device", 1, testLimits)
		var written atomic.Int64
		if withCounter {
			c.SetWireBytes(written.Load)
		}
		// The fake writer writes a 7-byte header and the frame encoding.
		send := func(f *ownsandv1.DataFrame) error {
			written.Add(7 + int64(proto.Size(f)))
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
		// An unmetered frame reports nothing.
		if err := s.Send(context.Background(), acceptedFrame(s)); err != nil {
			t.Fatal(err)
		}
		c.Close(ErrConnClosed)
		waitRun(t, done, time.Second)
		select {
		case n := <-got:
			t.Fatalf("an unmetered frame reported %d bytes", n)
		default:
		}
	}
}
