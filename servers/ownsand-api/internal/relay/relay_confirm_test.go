// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// confirmLimits keep the connection from closing as idle or sending
// keepalives while a test waits.
var confirmLimits = func() Limits {
	l := testLimits
	l.Keepalive = time.Minute
	l.IdleTimeout = time.Minute
	return l
}()

func confirmedFrame() *ownsandv1.DataFrame {
	return datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
}

type confirmResult struct {
	err error
	at  time.Time
}

func sendConfirmedAsync(ctx context.Context, s *Stream, f *ownsandv1.DataFrame) <-chan confirmResult {
	ch := make(chan confirmResult, 1)
	go func() {
		err := s.SendConfirmed(ctx, f, nil)
		ch <- confirmResult{err: err, at: time.Now()}
	}()
	return ch
}

// TestSendConfirmedReturnsAfterTheSend checks the success path and that the
// waiter registry is empty afterwards. SendConfirmed must not return while the
// writer is blocked in the send, although the queue has room.
func TestSendConfirmedReturnsAfterTheSend(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, confirmLimits)
	s := openStream(t, c)
	gate := make(chan struct{})
	sent := make(chan time.Time, 16)
	done := runConn(c, blockingRecv(c), func(f *ownsandv1.DataFrame) error {
		if f.GetStreamId() == s.ID && f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
			<-gate
			sent <- time.Now()
		}
		return nil
	})
	defer func() { c.Close(errors.New("test end")); <-done }()

	res := sendConfirmedAsync(context.Background(), s, confirmedFrame())
	select {
	case r := <-res:
		t.Fatalf("SendConfirmed returned %v before the send finished", r.err)
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	select {
	case r := <-res:
		if r.err != nil {
			t.Fatalf("SendConfirmed = %v, want nil", r.err)
		}
		if w := <-sent; r.at.Before(w) {
			t.Fatal("SendConfirmed returned before the send finished")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("SendConfirmed did not return after the send finished")
	}
	n := 0
	c.writeWaiters.Range(func(_, _ any) bool { n++; return true })
	if n != 0 {
		t.Fatalf("%d write waiters left after SendConfirmed returned", n)
	}
}

// TestSendConfirmedEndsWithTheContext checks that a context that ends while
// the writer is blocked ends SendConfirmed with the context error.
func TestSendConfirmedEndsWithTheContext(t *testing.T) {
	c := NewConn(context.Background(), "device", 1, confirmLimits)
	s := openStream(t, c)
	gate := make(chan struct{})
	t.Cleanup(func() { close(gate) })
	done := runConn(c, blockingRecv(c), func(f *ownsandv1.DataFrame) error {
		if f.GetStreamId() == s.ID && f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
			<-gate
		}
		return nil
	})
	defer func() { c.Close(errors.New("test end")); <-done }()

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	r := <-sendConfirmedAsync(ctx, s, confirmedFrame())
	if !errors.Is(r.err, context.DeadlineExceeded) {
		t.Fatalf("SendConfirmed = %v, want the context deadline error", r.err)
	}
}

// TestSendConfirmedFailsWhenTheConnectionCloses checks that closing the
// connection while the writer is blocked, and a failing send, both end
// SendConfirmed with ErrConnClosed.
func TestSendConfirmedFailsWhenTheConnectionCloses(t *testing.T) {
	t.Run("close while blocked", func(t *testing.T) {
		c := NewConn(context.Background(), "device", 1, confirmLimits)
		s := openStream(t, c)
		gate := make(chan struct{})
		t.Cleanup(func() { close(gate) })
		entered := make(chan struct{})
		done := runConn(c, blockingRecv(c), func(f *ownsandv1.DataFrame) error {
			if f.GetStreamId() == s.ID && f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
				close(entered)
				<-gate
			}
			return nil
		})
		defer func() { <-done }()
		res := sendConfirmedAsync(context.Background(), s, confirmedFrame())
		<-entered
		c.Close(errors.New("closed by the test"))
		r := <-res
		if !errors.Is(r.err, ErrConnClosed) {
			t.Fatalf("SendConfirmed = %v, want ErrConnClosed", r.err)
		}
	})
	t.Run("send fails", func(t *testing.T) {
		c := NewConn(context.Background(), "device", 1, confirmLimits)
		s := openStream(t, c)
		done := runConn(c, blockingRecv(c), func(f *ownsandv1.DataFrame) error {
			if f.GetStreamId() == s.ID && f.GetKind() == ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE {
				return errors.New("write failed")
			}
			return nil
		})
		defer func() { <-done }()
		r := <-sendConfirmedAsync(context.Background(), s, confirmedFrame())
		if !errors.Is(r.err, ErrConnClosed) {
			t.Fatalf("SendConfirmed = %v, want ErrConnClosed", r.err)
		}
	})
}
