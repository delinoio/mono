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
	ts.add("a", now.Add(time.Minute)) // a repeated revocation keeps the first tombstone
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

// A consumed tombstone leaves a stale queue entry; a new tombstone for the
// same ID must not be deleted by the stale entry reaching the head.
func TestTombstoneReaddAfterTake(t *testing.T) {
	var ts tombstones
	now := time.Unix(1000, 0)
	ts.add("a", now)
	ts.take("a", now)
	ts.add("a", now.Add(time.Minute))
	ts.add("b", now.Add(time.Minute))
	if !ts.has("a", now.Add(time.Minute)) || ts.len() != 2 {
		t.Fatalf("re-added tombstone lost, len = %d", ts.len())
	}
}
