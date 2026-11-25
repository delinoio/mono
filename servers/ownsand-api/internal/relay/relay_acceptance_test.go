// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const testAcceptanceWindow = 30 * time.Second

func writeAcceptanceRequest(t *testing.T, s *Stream, send func(*ownsandv1.DataFrame) error) <-chan error {
	t.Helper()
	f := datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.ExecRequest{})
	w := &confirmedWrite{stream: s, acceptance: testAcceptanceWindow, done: make(chan error, 1)}
	s.conn.writeWaiters.Store(f, w)
	defer s.conn.writeWaiters.Delete(f)
	if err := s.conn.sendMeasured(outFrame{frame: f}, send); err != nil {
		t.Fatal(err)
	}
	return w.done
}

func TestAcceptanceDeadlineStartsAtSendCompletion(t *testing.T) {
	clock := newStepClock()
	s := openStream(t, NewConnWithClock(context.Background(), "device", 1, testLimits, clock))
	written := writeAcceptanceRequest(t, s, func(*ownsandv1.DataFrame) error {
		clock.advance(time.Minute)
		if s.AcceptanceExpired() {
			t.Fatal("acceptance expired before the send completed")
		}
		return nil
	})
	clock.advance(testAcceptanceWindow - time.Nanosecond)
	if s.AcceptanceExpired() {
		t.Fatal("acceptance expired before the deadline")
	}
	clock.advance(time.Nanosecond)
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !s.AcceptanceExpired() {
		t.Fatal("reading the send result at the deadline kept the window open")
	}
}

func TestAcceptanceDeadlineUsesReceiptOrder(t *testing.T) {
	window := testAcceptanceWindow
	for _, tc := range []struct {
		name      string
		noticeAt  time.Duration
		firstSize int
		firstAt   time.Duration
		laterAt   time.Duration
		expired   bool
	}{
		{name: "immediately before deadline", noticeAt: window - time.Nanosecond},
		{name: "at deadline", noticeAt: window, expired: true},
		{name: "after deadline", noticeAt: window + time.Nanosecond, expired: true},
		{name: "extended by active stream", firstSize: LargeFrameBytes, firstAt: 20 * time.Second, noticeAt: 40 * time.Second, laterAt: 55 * time.Second},
		{name: "small frame does not extend", firstSize: 1024, firstAt: 20 * time.Second, noticeAt: 40 * time.Second, expired: true},
		{name: "large frame cannot reopen expired admission", firstSize: LargeFrameBytes, firstAt: window, noticeAt: window + time.Nanosecond, expired: true},
		{name: "later large frame cannot rescue late notice", noticeAt: window, laterAt: window + time.Nanosecond, expired: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			clock := newStepClock()
			c := NewConnWithClock(ctx, "device", 1, testLimits, clock)
			s := openStream(t, c)
			filler := openStream(t, c)
			receive := func(at time.Duration, f *ownsandv1.DataFrame) {
				clock.advance(at - clock.Elapsed())
				if err := c.dispatch(f); err != nil {
					t.Fatal(err)
				}
			}
			output := func(size int) *ownsandv1.DataFrame {
				return datapath.MustNewFrame(filler.ID, filler.SandboxID, &ownsandv1.ExecEvent{
					Event: &ownsandv1.ExecEvent_Stdout{Stdout: &ownsandv1.ExecOutput{Data: make([]byte, size)}},
				})
			}
			writeAcceptanceRequest(t, s, func(*ownsandv1.DataFrame) error { return nil })
			if tc.firstSize > 0 {
				receive(tc.firstAt, output(tc.firstSize))
			}
			notice := acceptedFrame(s)
			receive(tc.noticeAt, notice)
			if tc.laterAt > 0 {
				receive(tc.laterAt, output(LargeFrameBytes))
			}
			clock.advance(time.Minute)
			got, err := s.Next(ctx, nil)
			if err != nil || got != notice {
				t.Fatalf("Next = %v, %v, want the queued notice", got, err)
			}
			if got := s.AcceptanceExpired(); got != tc.expired {
				t.Errorf("AcceptanceExpired = %v, want %v", got, tc.expired)
			}
		})
	}
}
