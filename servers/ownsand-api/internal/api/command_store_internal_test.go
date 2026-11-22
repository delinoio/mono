// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"log/slog"
	"testing"
	"time"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
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
	if _, o := s.take("new", "dev", now.Add(time.Hour), grace); o != takeFound {
		t.Fatal("the record of the open channel was not eligible")
	}

	// A command stored on a channel that already closed expires like a
	// marked one, and an expired record that the sweep did not remove yet
	// counts as absent.
	s.add("late", commandInfo{deviceID: "dev", generation: oldConn.Generation}, oldConn, now)
	if _, o := s.take("late", "dev", now.Add(grace), grace); o != takeUnknown {
		t.Fatal("an expired record was eligible")
	}
	s.add("inside", commandInfo{deviceID: "dev", generation: oldConn.Generation}, oldConn, now)
	if _, o := s.take("inside", "dev", now.Add(grace-time.Second), grace); o != takeFound {
		t.Fatal("a record inside the grace period was not eligible")
	}

	// A removal after a failed enqueue is immediate.
	s.add("failed", commandInfo{deviceID: "dev", generation: newConn.Generation}, newConn, now)
	s.remove("failed")
	if _, o := s.take("failed", "dev", now, grace); o != takeUnknown {
		t.Fatal("a removed record was eligible")
	}
}

// TestCommandStoreTakeChecksDevice checks that a result from the wrong device
// leaves the live record in place and that the expected device consumes it
// exactly once.
func TestCommandStoreTakeChecksDevice(t *testing.T) {
	h := hub.New()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	grace := 2 * time.Minute
	var s commandStore

	conn := h.Register(context.Background(), "dev", "org")
	info := commandInfo{deviceID: "dev", generation: conn.Generation, sandboxID: "sb"}
	s.add("cmd", info, conn, now)

	for i := 0; i < 2; i++ {
		if _, o := s.take("cmd", "intruder", now, grace); o != takeWrongDevice {
			t.Fatalf("attempt %d: outcome = %v, want takeWrongDevice", i, o)
		}
		if s.len() != 1 {
			t.Fatalf("attempt %d: a rejected result removed the record", i)
		}
	}
	got, o := s.take("cmd", "dev", now, grace)
	if o != takeFound || got != info {
		t.Fatalf("expected device: outcome = %v, info = %+v", o, got)
	}
	if _, o := s.take("cmd", "dev", now, grace); o != takeUnknown {
		t.Fatalf("second take outcome = %v, want takeUnknown", o)
	}
	if _, o := s.take("cmd", "intruder", now, grace); o != takeUnknown {
		t.Fatalf("a take after consumption by the wrong device = %v, want takeUnknown", o)
	}

	// An expired record of another device is dropped like any expired record,
	// and a replacement channel of the same device stays eligible inside the
	// grace period.
	s.add("closed", info, conn, now)
	if n := s.markClosed("dev", conn.Generation, now); n != 1 {
		t.Fatalf("marked = %d, want 1", n)
	}
	if _, o := s.take("closed", "dev", now.Add(grace-time.Second), grace); o != takeFound {
		t.Fatalf("a replacement channel inside the grace period = %v, want takeFound", o)
	}
	s.add("expired", info, conn, now)
	s.markClosed("dev", conn.Generation, now)
	if _, o := s.take("expired", "intruder", now.Add(grace), grace); o != takeUnknown || s.len() != 0 {
		t.Fatalf("expired record: outcome = %v, retained = %d", o, s.len())
	}
}

// TestHandleCommandResultWrongDeviceKeepsPendingCommand checks that a result
// reported by a device other than the command's device neither consumes the
// record nor reaches the waiter, and that the right device still completes
// the command exactly once. The failing result of a pause command needs no
// database, so the service has none.
func TestHandleCommandResultWrongDeviceKeepsPendingCommand(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := &Service{Now: func() time.Time { return now }, Logger: slog.New(slog.DiscardHandler), Hub: hub.New()}
	owner := s.Hub.Register(context.Background(), "owner", "org")
	other := s.Hub.Register(context.Background(), "other", "org")
	s.commands.add("cmd", commandInfo{kind: cmdPauseSandbox, deviceID: "owner", generation: owner.Generation, orgID: "org", sandboxID: "sb"}, owner, now)
	waiter := s.Hub.Commands.Register("cmd")
	res := &ownsandv1.AgentCommandResult{CommandId: "cmd", Error: &ownsandv1.ErrorDetail{}}

	s.handleCommandResult(context.Background(), other, res)
	if n := s.commands.len(); n != 1 {
		t.Fatalf("records after the wrong device's result = %d, want 1", n)
	}
	select {
	case <-waiter:
		t.Fatal("the wrong device's result reached the waiter")
	default:
	}

	s.handleCommandResult(context.Background(), owner, res)
	if n := s.commands.len(); n != 0 {
		t.Fatalf("records after the right device's result = %d, want 0", n)
	}
	select {
	case got := <-waiter:
		if got != res {
			t.Fatalf("waiter result = %v, want %v", got, res)
		}
	default:
		t.Fatal("the right device's result did not reach the waiter")
	}

	// A repeated result finds no record and delivers nothing.
	again := s.Hub.Commands.Register("cmd")
	s.handleCommandResult(context.Background(), owner, res)
	select {
	case <-again:
		t.Fatal("a repeated result reached a waiter")
	default:
	}
}
