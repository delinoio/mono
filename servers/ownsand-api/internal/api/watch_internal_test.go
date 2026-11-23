// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"sync"
	"testing"
)

func closed(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestWatchLastUnregisterRemovesKey(t *testing.T) {
	var w sandboxWatch
	_, u1 := w.watch("a")
	_, u2 := w.watch("a")
	if w.count() != 2 {
		t.Fatalf("count = %d", w.count())
	}
	u1()
	if w.count() != 1 || len(w.m["a"]) != 1 {
		t.Fatalf("after first unregister: %d", w.count())
	}
	u2()
	if _, ok := w.m["a"]; ok || w.count() != 0 {
		t.Fatalf("key not removed: %v", w.m)
	}
	u2() // idempotent
}

func TestWatchUnregisterAfterFireIsNoop(t *testing.T) {
	var w sandboxWatch
	ch, u := w.watch("a")
	other, uo := w.watch("b")
	w.fire("a")
	if !closed(ch) {
		t.Fatal("fire did not close")
	}
	u()
	u()
	if w.count() != 1 || closed(other) {
		t.Fatalf("other waiter disturbed: count %d", w.count())
	}
	uo()
	if w.count() != 0 {
		t.Fatalf("count = %d", w.count())
	}
}

func TestWatchUnregisterKeepsOtherWaiterNotified(t *testing.T) {
	var w sandboxWatch
	c1, u1 := w.watch("a")
	c2, u2 := w.watch("a")
	u1()
	w.fire("a")
	if closed(c1) {
		t.Fatal("unregistered channel was closed")
	}
	if !closed(c2) {
		t.Fatal("remaining waiter lost its notification")
	}
	u2()
	if w.count() != 0 {
		t.Fatalf("count = %d", w.count())
	}
}

func TestWatchConcurrentFireAndUnregister(t *testing.T) {
	var w sandboxWatch
	const n = 200
	var wg sync.WaitGroup
	keep, keepUnreg := w.watch("a")
	for i := 0; i < n; i++ {
		_, u := w.watch("a")
		wg.Add(2)
		go func() { defer wg.Done(); u() }()
		go func() { defer wg.Done(); w.fire("a") }()
	}
	wg.Wait()
	// Check right after the workers finish, with no further fire: the
	// concurrent fires alone must have notified the retained watcher, and
	// every other registration must already be gone (fired or unregistered).
	select {
	case <-keep:
	default:
		t.Fatal("no concurrent fire notified the retained watcher")
	}
	if w.count() != 0 {
		t.Fatalf("count after concurrent fire and unregister = %d", w.count())
	}
	keepUnreg()
	if w.count() != 0 {
		t.Fatalf("count = %d", w.count())
	}
}
