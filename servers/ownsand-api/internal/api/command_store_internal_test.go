// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

func TestCommandStoreExpiry(t *testing.T) {
	h := hub.New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	var s commandStore

	oldConn := h.Register(context.Background(), "dev", "org")
	s.add("old", commandInfo{deviceID: "dev", generation: oldConn.Generation}, oldConn, now)
	s.add("other", commandInfo{deviceID: "other", generation: oldConn.Generation}, oldConn, now)
	// Registering the replacement closes the old channel.
	newConn := h.Register(context.Background(), "dev", "org")
	s.add("new", commandInfo{deviceID: "dev", generation: newConn.Generation}, newConn, now)

	// Only the records of the closed generation of the device are marked.
	if n := s.markClosed("dev", oldConn.Generation, now); n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	if n := s.markClosed("dev", oldConn.Generation, now); n != 0 {
		t.Fatalf("a second mark = %d, want 0", n)
	}
	if n := s.expire(now.Add(grace-time.Second), grace); n != 0 {
		t.Fatalf("expired inside the grace period = %d", n)
	}
	if n := s.expire(now.Add(grace), grace); n != 1 || s.len() != 2 {
		t.Fatalf("expired at the grace period = %d, retained = %d", n, s.len())
	}
	if _, ok := s.take("new", now.Add(time.Hour), grace); !ok {
		t.Fatal("the record of the open channel was not eligible")
	}

	// A command stored on a channel that already closed expires like a
	// marked one, and an expired record that the sweep did not remove yet
	// counts as absent.
	s.add("late", commandInfo{deviceID: "dev", generation: oldConn.Generation}, oldConn, now)
	if _, ok := s.take("late", now.Add(grace), grace); ok {
		t.Fatal("an expired record was eligible")
	}
	s.add("inside", commandInfo{deviceID: "dev", generation: oldConn.Generation}, oldConn, now)
	if _, ok := s.take("inside", now.Add(grace-time.Second), grace); !ok {
		t.Fatal("a record inside the grace period was not eligible")
	}

	// A removal after a failed enqueue is immediate.
	s.add("failed", commandInfo{deviceID: "dev", generation: newConn.Generation}, newConn, now)
	s.remove("failed")
	if _, ok := s.take("failed", now, grace); ok {
		t.Fatal("a removed record was eligible")
	}
}
