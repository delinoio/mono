// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// interruptibleSend returns a send that blocks until the returned interrupt
// runs, like a response write that a write deadline makes fail, and a flag
// that is set once a send returned.
func interruptibleSend() (send func(*ownsandv1.DataFrame) error, interrupt func() error, entered <-chan struct{}, returned *atomic.Bool) {
	release := make(chan struct{})
	enteredCh := make(chan struct{})
	var started atomic.Bool
	var done atomic.Bool
	send = func(*ownsandv1.DataFrame) error {
		if started.CompareAndSwap(false, true) {
			close(enteredCh)
		}
		<-release
		// A little work after the release: Run must still wait for it.
		time.Sleep(50 * time.Millisecond)
		done.Store(true)
		return errors.New("write deadline exceeded")
	}
	var once atomic.Bool
	interrupt = func() error {
		if once.CompareAndSwap(false, true) {
			close(release)
		}
		return nil
	}
	return send, interrupt, enteredCh, &done
}

func TestRunWaitsForInterruptedSend(t *testing.T) {
	// A long idle timeout keeps the idle check out of the way; the test
	// closes the connection itself.
	limits := testLimits
	limits.IdleTimeout = time.Minute
	c := NewConn(context.Background(), "device", 1, limits)
	send, interrupt, entered, returned := interruptibleSend()
	var interrupts atomic.Int32
	c.SetSendInterrupt(func() error {
		interrupts.Add(1)
		return interrupt()
	})
	done := runConn(c, blockingRecv(c), send)
	<-entered
	c.Close(ErrReplaced)
	if err := waitRun(t, done, 5*time.Second); !errors.Is(err, ErrReplaced) {
		t.Fatalf("Run = %v, want %v", err, ErrReplaced)
	}
	// Run returned, so the handler that called it may finish the response:
	// no send may still be running.
	if !returned.Load() {
		t.Fatal("Run returned while a send was still in flight")
	}
	if n := interrupts.Load(); n != 1 {
		t.Fatalf("interrupt ran %d times, want once", n)
	}
}

func TestRunInterruptsBlockedSendOfIdleConnection(t *testing.T) {
	// The clock stands still until the send is blocked, so the idle interval
	// starts only then.
	c, clk := newIdleTestConn(t)
	send, interrupt, entered, returned := interruptibleSend()
	// Release the blocked send when the test fails before Run interrupts it.
	t.Cleanup(func() { _ = interrupt() })
	c.SetSendInterrupt(interrupt)
	done := runConn(c, blockingRecv(c), send)
	awaitIdleSendEntry(t, entered, done)
	clk.advance(testLimits.IdleTimeout + time.Millisecond)
	if err := waitRun(t, done, 5*time.Second); !errors.Is(err, ErrIdle) {
		t.Fatalf("Run = %v, want %v", err, ErrIdle)
	}
	if !returned.Load() {
		t.Fatal("Run returned while a send was still in flight")
	}
}

func TestRunBoundsWaitWhenInterruptFails(t *testing.T) {
	limits := testLimits
	limits.IdleTimeout = time.Minute
	c := NewConn(context.Background(), "device", 1, limits)
	send, entered := blockedSend(t)
	c.SetSendInterrupt(func() error { return errors.New("interrupt not supported") })
	done := runConn(c, blockingRecv(c), send)
	<-entered
	c.Close(ErrReplaced)
	// The send stays blocked, so Run falls back to the bounded wait.
	if err := waitRun(t, done, time.Second); !errors.Is(err, ErrReplaced) {
		t.Fatalf("Run = %v, want %v", err, ErrReplaced)
	}
}
