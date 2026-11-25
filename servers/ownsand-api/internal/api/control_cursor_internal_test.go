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
	// handleErr, when set, returns the error that the delivery of the event
	// with the given ID returns; a failed delivery is not recorded.
	handleErr func(id int64) error
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
	h.c.handleEvent = func(_ context.Context, e events.Event) error {
		if h.onHandle != nil {
			h.onHandle(e.ID)
		}
		if h.handleErr != nil {
			if err := h.handleErr(e.ID); err != nil {
				return err
			}
		}
		h.delivered = append(h.delivered, e.ID)
		return nil
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

// interceptFirstLatestID makes the first latestID call run during, and
// return the result of, first. Later calls, including the ones that
// first causes through acquire, use the harness values; the nested calls
// therefore need no guard against re-entrancy.
func (h *cursorHarness) interceptFirstLatestID(first func() (int64, error)) {
	next := h.c.latestID
	calls := 0
	h.c.latestID = func(ctx context.Context) (int64, error) {
		if calls++; calls == 1 {
			return first()
		}
		return next(ctx)
	}
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

func TestControlCursorRetryEstablishesBaselineWithoutDeliveringEvents(t *testing.T) {
	h := newCursorHarness(t, 2)
	h.acquire(0, errors.New("database unavailable"))
	h.latest, h.latestErr = 2, nil
	h.c.processEvents(context.Background())
	if len(h.delivered) != 0 {
		t.Fatalf("delivered = %v, want none", h.delivered)
	}
	if got := h.cursor(); got != 2 {
		t.Fatalf("cursor = %d, want 2", got)
	}
}

func TestControlCursorInitSupersededByLeaseLost(t *testing.T) {
	h := newCursorHarness(t, 10)
	h.acquire(0, errors.New("database unavailable"))
	h.interceptFirstLatestID(func() (int64, error) {
		h.c.LeaseLost()
		return 5, nil
	})
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != -1 {
		t.Fatalf("cursor = %d, want -1 (the stale LatestID must not set the cursor after a loss)", got)
	}
	if len(h.delivered) != 0 {
		t.Fatalf("delivered = %v, want none", h.delivered)
	}
	h.latest, h.latestErr = 10, nil
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != -1 {
		t.Fatalf("cursor = %d, want -1 after polling without retry permission", got)
	}
}

func TestControlCursorInitSupersededByReacquire(t *testing.T) {
	h := newCursorHarness(t, 10)
	h.acquire(0, errors.New("database unavailable"))
	h.interceptFirstLatestID(func() (int64, error) {
		h.c.LeaseLost()
		h.acquire(8, nil)
		return 3, nil
	})
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 8 {
		t.Fatalf("cursor = %d, want 8 (the stale LatestID 3 must not replace it)", got)
	}
	if len(h.delivered) != 0 {
		t.Fatalf("delivered = %v, want none", h.delivered)
	}
	h.c.processEvents(context.Background())
	if want := []int64{9, 10}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v (events 4..8 are not replayed)", h.delivered, want)
	}
}

func TestControlCursorRetryCannotSeedNewPeriod(t *testing.T) {
	h := newCursorHarness(t, 10)
	h.c.S.Lease = &ownershipLease{held: true}
	h.c.S.BaselineReady = h.c.baselineReady
	readErr := errors.New("database unavailable")
	h.acquire(0, readErr)
	h.interceptFirstLatestID(func() (int64, error) {
		h.c.LeaseLost()
		h.acquire(0, readErr)
		return 2, nil
	})
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != -1 {
		t.Fatalf("new lease period got the old retry's cursor %d", got)
	}
	if ok, n := admit(h.c.S); ok || n != 0 {
		t.Fatalf("admitted = %v, registered = %d before the new baseline; want refused", ok, n)
	}
	h.latest, h.latestErr = 7, nil
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 7 {
		t.Fatalf("cursor = %d, want the new lease period's baseline 7", got)
	}
	if ok, n := admit(h.c.S); !ok || n != 1 {
		t.Fatalf("admitted = %v, registered = %d with the new baseline; want admitted once", ok, n)
	}
}

func TestControlCursorInitFailedReadLeavesStateUnchanged(t *testing.T) {
	h := newCursorHarness(t, 2)
	h.acquire(0, errors.New("database unavailable"))
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != -1 {
		t.Fatalf("cursor = %d, want -1 after a failed LatestID read", got)
	}
	h.c.mu.Lock()
	gen := h.c.cursorGen
	h.c.mu.Unlock()
	if gen != 1 {
		t.Fatalf("cursorGen = %d, want 1 after the failed retry", gen)
	}
	// The next poll repeats the read.
	h.latest, h.latestErr = 2, nil
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 2 {
		t.Fatalf("cursor = %d, want 2 after the retry", got)
	}
}

func TestControlCursorInitKeepsGeneration(t *testing.T) {
	h := newCursorHarness(t, 2)
	h.acquire(0, errors.New("database unavailable"))
	h.latest, h.latestErr = 2, nil
	h.c.processEvents(context.Background())
	h.c.mu.Lock()
	gen := h.c.cursorGen
	retry := h.c.retryBaseline
	h.c.mu.Unlock()
	if gen != 1 || retry {
		t.Fatalf("cursorGen = %d, retry allowed = %t; want 1, false after a successful retry", gen, retry)
	}
}

// TestControlProcessedCursor checks the cursor that the janitor reads: unset
// before the lease and after its loss or a failed read, the last processed
// delivery ID while the lease is held.
func TestControlProcessedCursor(t *testing.T) {
	h := newCursorHarness(t, 3)
	if _, ok := h.c.ProcessedCursor(); ok {
		t.Fatal("cursor reported before the lease was acquired")
	}
	h.acquire(1, nil)
	if id, ok := h.c.ProcessedCursor(); !ok || id != 1 {
		t.Fatalf("cursor after acquire = %d, %v; want 1, true (events up to the handover are not this holder's)", id, ok)
	}
	h.c.processEvents(context.Background())
	if id, ok := h.c.ProcessedCursor(); !ok || id != 3 {
		t.Fatalf("cursor after the batch = %d, %v; want 3, true", id, ok)
	}
	h.c.LeaseLost()
	if _, ok := h.c.ProcessedCursor(); ok {
		t.Fatal("cursor reported after the lease was lost")
	}
	h.acquire(0, errors.New("read failed"))
	if _, ok := h.c.ProcessedCursor(); ok {
		t.Fatal("cursor reported after a failed read at acquisition")
	}
	// Zero is a valid cursor: the table was empty at acquisition.
	h.acquire(0, nil)
	if id, ok := h.c.ProcessedCursor(); !ok || id != 0 {
		t.Fatalf("cursor with an empty table = %d, %v; want 0, true", id, ok)
	}
}

// ownershipLease is a LeaseHolder with a switchable ownership flag.
type ownershipLease struct{ held bool }

func (l *ownershipLease) Held() bool { return l.held }

func (l *ownershipLease) WhileHeld(fn func()) bool {
	if !l.held {
		return false
	}
	fn()
	return true
}

// admit runs the channel admission step of Service.admitChannel with a
// registration that counts its runs.
func admit(s *Service) (admitted bool, registered int) {
	admitted = s.admitChannel(func() { registered++ })
	return admitted, registered
}

// TestControlBaselineGatesChannelAdmission checks that a channel is refused
// while the lease is held but the control event baseline is unset, at the
// first acquisition and after a loss, and admitted once the event processor
// stored the baseline.
func TestControlBaselineGatesChannelAdmission(t *testing.T) {
	h := newCursorHarness(t, 0)
	lease := &ownershipLease{held: true}
	h.c.S.Lease = lease
	h.c.S.BaselineReady = h.c.baselineReady

	// The first read at acquisition fails: the lease is held, the cursor is
	// unset, and no channel is admitted or registered.
	h.acquire(0, errors.New("database unavailable"))
	if ok, n := admit(h.c.S); ok || n != 0 {
		t.Fatalf("admitted = %v, registered = %d with an unset baseline; want refused", ok, n)
	}

	// The retry by the event processor fails too, then succeeds.
	h.c.processEvents(context.Background())
	if ok, n := admit(h.c.S); ok || n != 0 {
		t.Fatalf("admitted = %v, registered = %d after a failed retry; want refused", ok, n)
	}
	h.latestErr = nil
	h.latest = 5
	h.c.processEvents(context.Background())
	if ok, n := admit(h.c.S); !ok || n != 1 {
		t.Fatalf("admitted = %v, registered = %d after the baseline; want admitted once", ok, n)
	}

	// A loss clears the baseline, and the next period starts refusing again
	// until its own baseline exists.
	h.c.LeaseLost()
	if ok, n := admit(h.c.S); ok || n != 0 {
		t.Fatalf("admitted = %v, registered = %d after the loss; want refused", ok, n)
	}
	h.acquire(5, nil)
	if ok, n := admit(h.c.S); !ok || n != 1 {
		t.Fatalf("admitted = %v, registered = %d after reacquisition; want admitted once", ok, n)
	}

	// Without the lease, a stored baseline admits nothing.
	lease.held = false
	if ok, n := admit(h.c.S); ok || n != 0 {
		t.Fatalf("admitted = %v, registered = %d without the lease; want refused", ok, n)
	}
}

// TestControlBaselineAdmissionRefusedWhileFirstReadPending checks that the
// baseline is also unset while the first read of the period is in flight, so
// a channel that connects during the read is refused.
func TestControlBaselineAdmissionRefusedWhileFirstReadPending(t *testing.T) {
	h := newCursorHarness(t, 0)
	h.c.S.Lease = &ownershipLease{held: true}
	h.c.S.BaselineReady = h.c.baselineReady
	refusedDuringRead := false
	h.interceptFirstLatestID(func() (int64, error) {
		ok, n := admit(h.c.S)
		refusedDuringRead = !ok && n == 0
		return 3, nil
	})
	h.latest = 3
	h.c.LeaseAcquired(context.Background())
	if !refusedDuringRead {
		t.Fatal("a channel was admitted while the first baseline read was in flight")
	}
	if ok, n := admit(h.c.S); !ok || n != 1 {
		t.Fatalf("admitted = %v, registered = %d after the read; want admitted once", ok, n)
	}
}

// TestControlBaselineAdmissionWithoutGate checks that a service without a
// control worker (nil BaselineReady) keeps admitting while the lease is held.
func TestControlBaselineAdmissionWithoutGate(t *testing.T) {
	s := &Service{Lease: &ownershipLease{held: true}}
	if ok, n := admit(s); !ok || n != 1 {
		t.Fatalf("admitted = %v, registered = %d without a gate; want admitted once", ok, n)
	}
}

// TestControlWiresBaselineGate checks that NewControl installs the gate, and
// that the gate follows the cursor of that control worker.
func TestControlWiresBaselineGate(t *testing.T) {
	s := &Service{Hub: hub.New(), Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Lease: &ownershipLease{held: true}}
	c := NewControl(s)
	c.latestID = func(context.Context) (int64, error) { return 4, nil }
	if s.BaselineReady == nil {
		t.Fatal("NewControl did not install the baseline gate")
	}
	if ok, _ := admit(s); ok {
		t.Fatal("a channel was admitted before the control worker stored a baseline")
	}
	c.LeaseAcquired(context.Background())
	if ok, n := admit(s); !ok || n != 1 {
		t.Fatalf("admitted = %v, registered = %d after the baseline; want admitted once", ok, n)
	}
}
