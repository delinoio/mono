// SPDX-License-Identifier: FSL-1.1-ALv2

package hub

import (
	"context"
	"sync"
	"testing"
	"time"
)

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
	go h.IfNoChannel("dev", func() {
		close(inFn)
		<-releaseFn
	})
	<-inFn

	registered := make(chan *Conn, 1)
	go func() { registered <- h.RegisterAdmitted(context.Background(), "dev", "org", own.admit) }()

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

	close(releaseFn)
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
