// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"fmt"
	"testing"
	"time"
)

// The tombstone set takes an explicit clock, so these tests never wait.
func TestTombstoneTakeConsumes(t *testing.T) {
	t.Parallel()
	var ts tombstones
	now := time.Unix(1000, 0)
	ts.add("a", now)
	if !ts.has("a", now) || ts.has("b", now) {
		t.Fatal("has: want a only")
	}
	if !ts.take("a", now) {
		t.Fatal("take did not find a")
	}
	if ts.take("a", now) || ts.len() != 0 {
		t.Fatal("take did not consume the tombstone")
	}
}

func TestTombstoneExpiresAfterSessionLifetime(t *testing.T) {
	t.Parallel()
	var ts tombstones
	now := time.Unix(1000, 0)
	ts.add("a", now)
	ts.add("b", now.Add(30*time.Minute))
	ts.add("a", now.Add(45*time.Minute)) // A repeated revocation keeps the first expiry.
	if !ts.has("a", now.Add(time.Hour-time.Nanosecond)) {
		t.Fatal("tombstone expired before the session lifetime")
	}
	if ts.has("a", now.Add(time.Hour)) {
		t.Fatal("tombstone outlived the session lifetime")
	}
	if !ts.has("b", now.Add(time.Hour)) {
		t.Fatal("a later tombstone expired with an earlier one")
	}
	if ts.has("b", now.Add(time.Hour+30*time.Minute)) || ts.len() != 0 {
		t.Fatal("tombstone b did not expire")
	}
}

func TestTombstoneCapDropsOldest(t *testing.T) {
	t.Parallel()
	const capacity = 1024
	var ts tombstones
	now := time.Unix(1000, 0)
	for i := 0; i < capacity; i++ {
		ts.add(fmt.Sprintf("s%d", i), now)
	}
	if got := ts.len(); got != capacity {
		t.Fatalf("len at capacity = %d, want %d", got, capacity)
	}
	for i := 0; i < capacity; i++ {
		if !ts.has(fmt.Sprintf("s%d", i), now) {
			t.Fatalf("tombstone s%d was dropped at capacity", i)
		}
	}
	for i := 0; i < 3; i++ {
		ts.add(fmt.Sprintf("s%d", capacity+i), now)
		if got := ts.len(); got != capacity {
			t.Fatalf("len after %d extra inserts = %d, want %d", i+1, got, capacity)
		}
		if ts.has(fmt.Sprintf("s%d", i), now) {
			t.Fatalf("oldest tombstone s%d survived the cap", i)
		}
	}
	for i := 3; i < capacity+3; i++ {
		if !ts.has(fmt.Sprintf("s%d", i), now) {
			t.Fatalf("newer tombstone s%d was dropped", i)
		}
	}
}

func TestTombstoneTakeRemovesQueueEntryOutOfOrder(t *testing.T) {
	t.Parallel()
	var ts tombstones
	now := time.Unix(1000, 0)
	for i := 0; i < maxTombstones; i++ {
		ts.add(fmt.Sprintf("s%d", i), now)
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("s%d", i)
		if !ts.take(id, now) {
			t.Fatalf("take did not find %s", id)
		}
		if got := ts.queue.Len(); got != maxTombstones-1 {
			t.Fatalf("queue length after take = %d, want %d", got, maxTombstones-1)
		}
		ts.add(id, now)
		if got := ts.queue.Len(); got != maxTombstones {
			t.Fatalf("queue length after re-add = %d, want %d", got, maxTombstones)
		}
	}
	ts.add("new", now)
	if ts.has("s0", now) || !ts.has("s4", now) {
		t.Fatal("capacity eviction did not drop the oldest live record")
	}
}

func TestTombstoneReaddAfterTake(t *testing.T) {
	t.Parallel()
	var ts tombstones
	now := time.Unix(1000, 0)
	ts.add("head", now)
	ts.add("a", now)
	if !ts.take("a", now) {
		t.Fatal("take did not find a")
	}
	ts.add("a", now.Add(time.Minute))
	if !ts.has("a", now.Add(time.Hour)) || ts.len() != 1 {
		t.Fatalf("re-added tombstone did not survive the original expiry, len = %d", ts.len())
	}
	if !ts.has("a", now.Add(time.Hour+time.Minute-time.Nanosecond)) {
		t.Fatal("re-added tombstone expired before its own expiry")
	}
	if ts.has("a", now.Add(time.Hour+time.Minute)) {
		t.Fatal("re-added tombstone outlived its own expiry")
	}
}
