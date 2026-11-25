// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"fmt"
	"testing"
	"time"
)

// The tombstone set takes an explicit clock, so these tests never wait.
func TestTombstoneTakeConsumes(t *testing.T) {
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
	var ts tombstones
	now := time.Unix(1000, 0)
	ts.add("a", now)
	ts.add("b", now.Add(30*time.Minute))
	ts.add("a", now.Add(45*time.Minute)) // A repeated revocation keeps the first expiry.
	if !ts.has("a", now.Add(tombstoneTTL-time.Nanosecond)) {
		t.Fatal("tombstone expired before the session lifetime")
	}
	if ts.has("a", now.Add(tombstoneTTL)) {
		t.Fatal("tombstone outlived the session lifetime")
	}
	if !ts.has("b", now.Add(tombstoneTTL)) {
		t.Fatal("a later tombstone expired with an earlier one")
	}
	if ts.has("b", now.Add(30*time.Minute+tombstoneTTL)) || ts.len() != 0 {
		t.Fatal("tombstone b did not expire")
	}
}

func TestTombstoneCapDropsOldest(t *testing.T) {
	var ts tombstones
	now := time.Unix(1000, 0)
	for i := 0; i < maxTombstones+3; i++ {
		ts.add(fmt.Sprintf("s%d", i), now)
	}
	if ts.len() != maxTombstones {
		t.Fatalf("len = %d, want %d", ts.len(), maxTombstones)
	}
	for i := 0; i < 3; i++ {
		if ts.has(fmt.Sprintf("s%d", i), now) {
			t.Fatalf("oldest tombstone s%d survived the cap", i)
		}
	}
	if !ts.has("s3", now) || !ts.has(fmt.Sprintf("s%d", maxTombstones+2), now) {
		t.Fatal("a newer tombstone was dropped")
	}
}

func TestTombstoneTakeRemovesQueueEntryOutOfOrder(t *testing.T) {
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
	var ts tombstones
	now := time.Unix(1000, 0)
	ts.add("head", now)
	ts.add("a", now)
	if !ts.take("a", now) {
		t.Fatal("take did not find a")
	}
	ts.add("a", now.Add(time.Minute))
	if !ts.has("a", now.Add(tombstoneTTL)) || ts.len() != 1 {
		t.Fatalf("re-added tombstone did not survive the original expiry, len = %d", ts.len())
	}
	if ts.has("a", now.Add(tombstoneTTL+time.Minute)) {
		t.Fatal("re-added tombstone outlived its own expiry")
	}
}
