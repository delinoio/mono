// SPDX-License-Identifier: FSL-1.1-ALv2

package hub

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

func ack(seq uint64) *ownsandv1.AgentEnvelope {
	return &ownsandv1.AgentEnvelope{Message: &ownsandv1.AgentEnvelope_HeartbeatAck{HeartbeatAck: &ownsandv1.HeartbeatAck{Sequence: seq}}}
}

// TestSendClosesSaturatedChannel checks that a Send on a full queue that
// no writer drains fails after the queue-full wait and closes the channel
// with ErrQueueFull.
func TestSendClosesSaturatedChannel(t *testing.T) {
	h := New()
	h.QueueFullWait = 50 * time.Millisecond
	conn := h.Register(context.Background(), "device", "org")
	for i := 0; i < outboundQueue; i++ {
		if err := conn.Send(context.Background(), ack(uint64(i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	start := time.Now()
	err := conn.Send(context.Background(), ack(outboundQueue))
	if !errors.Is(err, ErrQueueFull) {
		t.Fatalf("send on a full queue = %v, want ErrQueueFull", err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("send on a full queue took %v, want about the queue-full wait", elapsed)
	}
	select {
	case <-conn.Done():
	default:
		t.Fatal("the saturated channel stayed open")
	}
	if cause := context.Cause(conn.Context()); !errors.Is(cause, ErrQueueFull) {
		t.Fatalf("close cause = %v, want ErrQueueFull", cause)
	}
	if err := conn.Send(context.Background(), ack(0)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on the closed channel = %v, want ErrChannelClosed", err)
	}
}

// TestSendWaitsForADrainingWriter checks that a burst over the queue size
// succeeds in order while the writer drains, and does not close the channel.
func TestSendWaitsForADrainingWriter(t *testing.T) {
	h := New()
	h.QueueFullWait = 5 * time.Second
	conn := h.Register(context.Background(), "device", "org")
	const total = outboundQueue*3 + 7
	got := make(chan uint64, total)
	go func() {
		for env := range conn.Outbound() {
			time.Sleep(100 * time.Microsecond)
			got <- env.GetHeartbeatAck().GetSequence()
		}
	}()
	for i := 0; i < total; i++ {
		if err := conn.Send(context.Background(), ack(uint64(i))); err != nil {
			t.Fatalf("send %d: %v", i, err)
		}
	}
	for i := 0; i < total; i++ {
		select {
		case seq := <-got:
			if seq != uint64(i) {
				t.Fatalf("envelope %d has sequence %d", i, seq)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("envelope %d not written", i)
		}
	}
	select {
	case <-conn.Done():
		t.Fatalf("the channel closed: %v", context.Cause(conn.Context()))
	default:
	}
}

// TestSendStopsOnCallerContext checks that the caller's context still ends
// the wait without closing the channel.
func TestSendStopsOnCallerContext(t *testing.T) {
	h := New()
	conn := h.Register(context.Background(), "device", "org")
	for i := 0; i < outboundQueue; i++ {
		if err := conn.Send(context.Background(), ack(uint64(i))); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := conn.Send(ctx, ack(0)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send = %v, want the context error", err)
	}
	select {
	case <-conn.Done():
		t.Fatal("a caller timeout closed the channel")
	default:
	}
}

// TestSendRejectsEndedContextWithRoom checks that a Send whose context
// already ended returns the context error and queues nothing, also when the
// outbound queue has room, and leaves the channel open.
func TestSendRejectsEndedContextWithRoom(t *testing.T) {
	h := New()
	conn := h.Register(context.Background(), "device", "org")
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := conn.Send(canceled, ack(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("send with a canceled context = %v, want context.Canceled", err)
	}
	expired, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if err := conn.Send(expired, ack(2)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("send with an expired context = %v, want context.DeadlineExceeded", err)
	}
	select {
	case env := <-conn.Outbound():
		t.Fatalf("an envelope was queued for an ended context: %v", env)
	default:
	}
	select {
	case <-conn.Done():
		t.Fatal("a caller context error closed the channel")
	default:
	}
	// The channel still accepts a Send with a live context.
	if err := conn.Send(context.Background(), ack(3)); err != nil {
		t.Fatalf("send after the rejected sends: %v", err)
	}
}

// TestSendClosedChannelWinsOverEndedContext checks that the channel-closed
// result is unchanged when the caller's context also ended.
func TestSendClosedChannelWinsOverEndedContext(t *testing.T) {
	h := New()
	conn := h.Register(context.Background(), "device", "org")
	conn.Close(errors.New("test close"))
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := conn.Send(canceled, ack(1)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on a closed channel with a canceled context = %v, want ErrChannelClosed", err)
	}
}

// TestSaturatedCloseKeepsReplacement checks that the saturation close of an
// old channel leaves its replacement current.
func TestSaturatedCloseKeepsReplacement(t *testing.T) {
	h := New()
	h.QueueFullWait = 20 * time.Millisecond
	old := h.Register(context.Background(), "device", "org")
	for i := 0; i < outboundQueue; i++ {
		if err := old.Send(context.Background(), ack(uint64(i))); err != nil {
			t.Fatal(err)
		}
	}
	replacement := h.Register(context.Background(), "device", "org")
	// The replacement already closed the old channel, so Send reports the
	// close and keeps that cause.
	if err := old.Send(context.Background(), ack(0)); !errors.Is(err, ErrChannelClosed) {
		t.Fatalf("send on the replaced channel = %v", err)
	}
	if h.Unregister(old) {
		t.Fatal("the old channel counted as current")
	}
	if h.Get("device") != replacement {
		t.Fatal("the replacement is not current")
	}
}

// ownershipLock is a stand-in for the lease's ownership lock: admit runs fn
// under the lock, like Lease.WhileHeld.
type ownershipLock struct {
	mu      sync.Mutex
	entered chan struct{}
}

func (o *ownershipLock) admit(fn func()) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	close(o.entered)
	fn()
	return true
}

// TestRegisterAdmittedWaitsForFenceOutsideOwnershipLock checks that an
// admission that waits for the device fence, which IfNoChannel holds across
// database I/O, does not hold the ownership lock while it waits, and that
// it registers the channel under the lock after the fence is free.
func TestRegisterAdmittedWaitsForFenceOutsideOwnershipLock(t *testing.T) {
	h := New()
	own := &ownershipLock{entered: make(chan struct{})}

	inFn := make(chan struct{})
	releaseFn := make(chan struct{})
	fnDone := make(chan struct{})
	registrarDone := make(chan struct{})
	// A fatal assertion ends the test with runtime.Goexit before the normal
	// release, so the cleanup releases the fence callback and waits for both
	// workers; otherwise they would stay blocked after the test fails.
	release := sync.OnceFunc(func() { close(releaseFn) })
	t.Cleanup(func() {
		release()
		for name, done := range map[string]chan struct{}{"fence holder": fnDone, "registrar": registrarDone} {
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Errorf("the %s goroutine did not finish after the device fence was released", name)
			}
		}
	})
	go func() {
		defer close(fnDone)
		h.IfNoChannel("dev", func() {
			close(inFn)
			<-releaseFn
		})
	}()
	<-inFn

	registered := make(chan *Conn, 1)
	go func() {
		defer close(registrarDone)
		registered <- h.RegisterAdmitted(context.Background(), "dev", "org", own.admit)
	}()

	// The admission waits for the fence; the ownership lock stays free for
	// the lease renewal and the loss transition, and fn was not run.
	deadline := time.After(10 * time.Second)
	for {
		h.mu.Lock()
		refs := h.fences["dev"].refs
		h.mu.Unlock()
		if refs == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("the admission did not wait for the device fence")
		case <-time.After(time.Millisecond):
		}
	}
	if !own.mu.TryLock() {
		t.Fatal("the ownership lock is held while the admission waits for the device fence")
	}
	own.mu.Unlock()
	select {
	case <-own.entered:
		t.Fatal("admit ran while the device fence was held")
	default:
	}
	if h.Get("dev") != nil {
		t.Fatal("a channel was registered while the device fence was held")
	}

	release()
	select {
	case c := <-registered:
		if c == nil || h.Get("dev") != c {
			t.Fatalf("the admission did not register its channel: %v", c)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admission did not finish after the device fence was released")
	}
}

// TestRegisterAdmittedRefusedLeavesHubUnchanged checks that a refused
// admission registers nothing and releases the device fence.
func TestRegisterAdmittedRefusedLeavesHubUnchanged(t *testing.T) {
	h := New()
	old := h.Register(context.Background(), "dev", "org")
	if c := h.RegisterAdmitted(context.Background(), "dev", "org", func(func()) bool { return false }); c != nil {
		t.Fatalf("a refused admission returned a channel: %v", c)
	}
	if h.Get("dev") != old || old.Context().Err() != nil {
		t.Fatal("a refused admission changed the registered channel")
	}
	if len(h.fences) != 0 {
		t.Fatal("a refused admission kept the device fence")
	}
	if !h.IfNoChannel("other", func() {}) {
		t.Fatal("the fence of another device is blocked")
	}
}
