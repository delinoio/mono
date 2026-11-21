// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
	"github.com/delinoio/mono/servers/ownsand-api/internal/hub"
)

// cursorHarness drives Control.processEvents without a database. readEvents
// serves the events with an ID above the cursor in pages, and every
// delivered event ID is recorded.
type cursorHarness struct {
	c         *Control
	all       []events.Event
	delivered []int64
	latest    int64
	latestErr error
	// onRead and onHandle run inside the batch, before the read and before
	// the delivery of the event with the given ID, to interleave lease
	// transitions with it.
	onRead   func(after int64)
	onHandle func(id int64)
}

// newCursorHarness serves the events 1..n; the processor reads 500 per page.
func newCursorHarness(t *testing.T, n int64) *cursorHarness {
	t.Helper()
	h := &cursorHarness{}
	for id := int64(1); id <= n; id++ {
		h.all = append(h.all, events.Event{ID: id, Kind: events.KindDeviceConfigChanged})
	}
	s := &Service{Hub: hub.New(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	h.c = &Control{S: s, kick: make(chan struct{}, 1), cursor: -1}
	h.c.latestID = func(context.Context) (int64, error) { return h.latest, h.latestErr }
	h.c.readEvents = func(_ context.Context, after int64, limit int) ([]events.Event, error) {
		if h.onRead != nil {
			h.onRead(after)
		}
		var out []events.Event
		for _, e := range h.all {
			if e.ID > after && len(out) < limit {
				out = append(out, e)
			}
		}
		return out, nil
	}
	h.c.handleEvent = func(_ context.Context, e events.Event) {
		if h.onHandle != nil {
			h.onHandle(e.ID)
		}
		h.delivered = append(h.delivered, e.ID)
	}
	return h
}

func (h *cursorHarness) cursor() int64 {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	return h.c.cursor
}

// acquire runs LeaseAcquired with the given newest event ID.
func (h *cursorHarness) acquire(latest int64, err error) {
	h.latest, h.latestErr = latest, err
	h.c.LeaseAcquired(context.Background())
}

func equalIDs(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestControlCursorHeldBatchAdvances(t *testing.T) {
	h := newCursorHarness(t, 1200)
	h.acquire(0, nil)
	h.c.processEvents(context.Background())
	if len(h.delivered) != 1200 || h.delivered[0] != 1 || h.delivered[1199] != 1200 {
		t.Fatalf("delivered %d events, want 1..1200 across three pages", len(h.delivered))
	}
	if got := h.cursor(); got != 1200 {
		t.Fatalf("cursor = %d, want 1200", got)
	}
}

func TestControlCursorLossLeavesUnset(t *testing.T) {
	h := newCursorHarness(t, 3)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 2 {
			h.c.LeaseLost()
		}
	}
	h.c.processEvents(context.Background())
	if want := []int64{1, 2}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v (deliveries that returned stay, no later one)", h.delivered, want)
	}
	if got := h.cursor(); got != -1 {
		t.Fatalf("cursor = %d, want -1", got)
	}
}

func TestControlCursorReacquireKeepsLatestID(t *testing.T) {
	// The loss and the reacquisition both happen while the batch delivers
	// event 1; the rest of the page and the later pages, including events
	// above the new LatestID, must not be delivered, and
	// the cursor stays at the newest ID of the new lease period.
	h := newCursorHarness(t, 1200)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 1 {
			h.c.LeaseLost()
			h.acquire(2, nil)
		}
	}
	h.c.processEvents(context.Background())
	if want := []int64{1}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v", h.delivered, want)
	}
	if got := h.cursor(); got != 2 {
		t.Fatalf("cursor = %d, want the new LatestID 2", got)
	}
}

func TestControlCursorReacquireBetweenPages(t *testing.T) {
	h := newCursorHarness(t, 1200)
	h.acquire(0, nil)
	reads := 0
	h.onRead = func(int64) {
		if reads++; reads == 2 {
			h.c.LeaseLost()
			h.acquire(700, nil)
		}
	}
	h.c.processEvents(context.Background())
	// The first page (1..500) was delivered and stored; the second read
	// runs after the loss and the reacquisition, so nothing more follows.
	if len(h.delivered) != 500 || h.delivered[499] != 500 {
		t.Fatalf("delivered %d events, want exactly the first page", len(h.delivered))
	}
	if got := h.cursor(); got != 700 {
		t.Fatalf("cursor = %d, want the new LatestID 700", got)
	}
}

func TestControlCursorReacquireWithZeroLatestID(t *testing.T) {
	h := newCursorHarness(t, 2)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 1 {
			h.c.LeaseLost()
			h.acquire(0, nil)
		}
	}
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0 (a zero LatestID is kept)", got)
	}
	if want := []int64{1}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v", h.delivered, want)
	}
}

func TestControlCursorFailedLatestIDRead(t *testing.T) {
	h := newCursorHarness(t, 2)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 1 {
			h.c.LeaseLost()
			h.acquire(7, errors.New("database unavailable"))
		}
	}
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != -1 {
		t.Fatalf("cursor = %d, want -1 after a failed LatestID read", got)
	}
	if want := []int64{1}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v", h.delivered, want)
	}
}

func TestControlCursorReacquireWithEqualCursor(t *testing.T) {
	// The reacquired LatestID equals the cursor that the stale batch would
	// publish; the stale batch still delivers nothing more.
	h := newCursorHarness(t, 3)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 1 {
			h.c.LeaseLost()
			h.acquire(0, nil)
		}
	}
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0", got)
	}
	if want := []int64{1}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v", h.delivered, want)
	}
	// Reacquire without an intervening loss also supersedes the batch.
	h2 := newCursorHarness(t, 3)
	h2.acquire(0, nil)
	h2.onHandle = func(id int64) {
		if id == 1 {
			h2.acquire(1, nil)
		}
	}
	h2.c.processEvents(context.Background())
	if got := h2.cursor(); got != 1 {
		t.Fatalf("cursor = %d, want 1", got)
	}
	if want := []int64{1}; !equalIDs(h2.delivered, want) {
		t.Fatalf("delivered = %v, want %v", h2.delivered, want)
	}
}

func TestControlCursorUnsetCursorAcquiresWithoutWriteBack(t *testing.T) {
	h := newCursorHarness(t, 2)
	h.latest = 2
	h.c.processEvents(context.Background())
	if len(h.delivered) != 0 {
		t.Fatalf("delivered = %v, want none", h.delivered)
	}
	if got := h.cursor(); got != 2 {
		t.Fatalf("cursor = %d, want 2", got)
	}
}
