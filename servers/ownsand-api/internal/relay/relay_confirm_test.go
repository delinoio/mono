// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

const confirmTimeout = 2 * time.Second

type confirmedSendFixture struct {
	conn           *Conn
	entered        chan struct{}
	writeCompleted chan struct{}
	returned       chan struct{}
	releaseWriter  func()
	result         error // Read only after returned is closed.
}

func startConfirmedSend(t *testing.T, ctx context.Context, writeErr error) *confirmedSendFixture {
	t.Helper()
	limits := testLimits
	// Keep the idle timer and keepalives outside the fixture's deadlines.
	limits.Keepalive = time.Minute
	limits.IdleTimeout = time.Minute
	c := NewConn(context.Background(), "device", 1, limits)
	gate := make(chan struct{})
	fixture := &confirmedSendFixture{
		conn:           c,
		entered:        make(chan struct{}),
		writeCompleted: make(chan struct{}),
		returned:       make(chan struct{}),
		releaseWriter:  sync.OnceFunc(func() { close(gate) }),
	}

	var runDone <-chan error
	t.Cleanup(func() {
		c.Close(ErrConnClosed)
		fixture.releaseWriter()
		if runDone == nil {
			return
		}
		select {
		case <-fixture.returned:
		case <-time.After(confirmTimeout):
			t.Error("SendConfirmed did not stop during teardown")
		}
		waitRun(t, runDone, confirmTimeout)
	})

	openCtx, cancelOpen := context.WithTimeout(t.Context(), confirmTimeout)
	defer cancelOpen()
	s, err := c.Open(openCtx, "grant", "sandbox")
	if err != nil {
		t.Fatalf("open stream for SendConfirmed: %v", err)
	}
	frame := datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})
	runDone = runConn(c, blockingRecv(c), func(f *ownsandv1.DataFrame) error {
		if f != frame {
			return nil
		}
		close(fixture.entered)
		<-gate
		close(fixture.writeCompleted)
		return writeErr
	})
	go func() {
		defer close(fixture.returned)
		fixture.result = s.SendConfirmed(ctx, frame, nil, 0)
	}()
	return fixture
}

func awaitConfirmSignal(t *testing.T, signal <-chan struct{}, stage string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(confirmTimeout):
		t.Fatalf("timed out waiting for %s", stage)
	}
}

func (f *confirmedSendFixture) requirePending(t *testing.T) {
	t.Helper()
	awaitConfirmSignal(t, f.entered, "matching transport write entry")
	// Let the sender reach its wait or return while the writer is still gated.
	synctest.Wait()
	select {
	case <-f.returned:
		t.Fatalf("SendConfirmed returned %v while the matching transport write was blocked", f.result)
	default:
	}
}

func (f *confirmedSendFixture) requireResult(t *testing.T, want error) {
	t.Helper()
	awaitConfirmSignal(t, f.returned, "SendConfirmed result")
	if !errors.Is(f.result, want) {
		t.Fatalf("SendConfirmed = %v, want %v", f.result, want)
	}
}

func TestSendConfirmedReturnsAfterTheSend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := startConfirmedSend(t, t.Context(), nil)
		f.requirePending(t)
		f.releaseWriter()
		awaitConfirmSignal(t, f.writeCompleted, "matching transport write completion")
		f.requireResult(t, nil)

		n := 0
		f.conn.writeWaiters.Range(func(_, _ any) bool {
			n++
			return true
		})
		if n != 0 {
			t.Fatalf("%d write waiters left after SendConfirmed returned", n)
		}
	})
}

func TestSendConfirmedEndsWithTheContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		f := startConfirmedSend(t, ctx, nil)
		f.requirePending(t)
		cancel()
		f.requireResult(t, context.Canceled)
	})
}

func TestSendConfirmedFailsWhenTheConnectionCloses(t *testing.T) {
	t.Run("close while blocked", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := startConfirmedSend(t, t.Context(), nil)
			f.requirePending(t)
			f.conn.Close(errors.New("closed by the test"))
			f.requireResult(t, ErrConnClosed)
		})
	})
	t.Run("send fails", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := startConfirmedSend(t, t.Context(), errors.New("write failed"))
			f.requirePending(t)
			f.releaseWriter()
			awaitConfirmSignal(t, f.writeCompleted, "matching transport write completion")
			f.requireResult(t, ErrConnClosed)
		})
	})
}
