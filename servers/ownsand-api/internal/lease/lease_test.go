// SPDX-License-Identifier: FSL-1.1-ALv2

package lease

import (
	"io"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"
)

func newTestLease() (*Lease, *atomic.Int32) {
	var lost atomic.Int32
	l := &Lease{
		InstanceID: "test",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnLose:     func() { lost.Add(1) },
	}
	return l, &lost
}

// TestWhileHeldRunsOnlyWhileHeld checks that fn is not run without the lease.
func TestWhileHeldRunsOnlyWhileHeld(t *testing.T) {
	l, _ := newTestLease()
	ran := false
	if l.WhileHeld(func() { ran = true }) || ran {
		t.Fatal("fn ran without the lease")
	}
	l.set(true)
	if !l.WhileHeld(func() { ran = true }) || !ran {
		t.Fatal("fn did not run with the lease")
	}
	l.set(false)
	ran = false
	if l.WhileHeld(func() { ran = true }) || ran {
		t.Fatal("fn ran after the lease was lost")
	}
}

// TestWhileHeldSerializesWithLoss checks that the loss transition waits for
// a running fn and runs OnLose only after it, so OnLose sees the effects of
// fn, and that a call after the transition does not run fn.
func TestWhileHeldSerializesWithLoss(t *testing.T) {
	l, lost := newTestLease()
	l.set(true)

	inFn := make(chan struct{})
	releaseFn := make(chan struct{})
	whileHeldDone := make(chan bool, 1)
	go func() {
		whileHeldDone <- l.WhileHeld(func() {
			close(inFn)
			<-releaseFn
		})
	}()
	<-inFn

	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		l.set(false)
	}()
	// The loss transition is blocked on the ownership lock held by fn.
	select {
	case <-setDone:
		t.Fatal("the loss transition did not wait for the running fn")
	case <-time.After(100 * time.Millisecond):
	}
	if got := lost.Load(); got != 0 {
		t.Fatalf("OnLose ran %d times before fn returned", got)
	}

	close(releaseFn)
	if !<-whileHeldDone {
		t.Fatal("WhileHeld reported that fn did not run")
	}
	select {
	case <-setDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the loss transition stayed blocked")
	}
	if got := lost.Load(); got != 1 {
		t.Fatalf("OnLose ran %d times, want 1", got)
	}
	if l.WhileHeld(func() { t.Error("fn ran after the loss transition") }) {
		t.Fatal("WhileHeld ran fn after the loss transition")
	}
}
