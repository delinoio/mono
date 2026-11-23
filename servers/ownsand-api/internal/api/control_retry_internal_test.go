// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/servers/ownsand-api/internal/events"
)

var errInjectedDelivery = errors.New("injected delivery failure")

// failOnce makes the delivery of the event with the given ID fail until the
// returned function is called.
func failOnce(h *cursorHarness, id int64) (clear func()) {
	failing := true
	h.handleErr = func(got int64) error {
		if failing && got == id {
			return errInjectedDelivery
		}
		return nil
	}
	return func() { failing = false }
}

func TestControlRetryStopsAtFailedEventAndResumesThere(t *testing.T) {
	h := newCursorHarness(t, 5)
	h.acquire(0, nil)
	clear := failOnce(h, 3)
	h.c.processEvents(context.Background())
	if want := []int64{1, 2}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v (the batch stops at event 3)", h.delivered, want)
	}
	if got := h.cursor(); got != 2 {
		t.Fatalf("cursor = %d, want 2 (the event before the failed one)", got)
	}

	// While the failure lasts, every poll stops at the same event and
	// delivers nothing twice.
	h.c.processEvents(context.Background())
	if want := []int64{1, 2}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered after a second poll = %v, want %v", h.delivered, want)
	}

	clear()
	h.c.processEvents(context.Background())
	if want := []int64{1, 2, 3, 4, 5}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered after recovery = %v, want %v (prefix once, suffix after recovery)", h.delivered, want)
	}
	if got := h.cursor(); got != 5 {
		t.Fatalf("cursor = %d, want 5", got)
	}
}

func TestControlRetryFirstEventKeepsCursor(t *testing.T) {
	h := newCursorHarness(t, 3)
	h.acquire(0, nil)
	failOnce(h, 1)
	h.c.processEvents(context.Background())
	if len(h.delivered) != 0 {
		t.Fatalf("delivered = %v, want none", h.delivered)
	}
	if got := h.cursor(); got != 0 {
		t.Fatalf("cursor = %d, want 0", got)
	}
}

func TestControlRetryOnLaterPageStoresEarlierPages(t *testing.T) {
	h := newCursorHarness(t, 1200)
	h.acquire(0, nil)
	clear := failOnce(h, 700)
	h.c.processEvents(context.Background())
	if len(h.delivered) != 699 || h.delivered[698] != 699 {
		t.Fatalf("delivered %d events, want 1..699", len(h.delivered))
	}
	if got := h.cursor(); got != 699 {
		t.Fatalf("cursor = %d, want 699", got)
	}
	clear()
	h.c.processEvents(context.Background())
	if len(h.delivered) != 1200 || h.delivered[699] != 700 || h.delivered[1199] != 1200 {
		t.Fatalf("delivered %d events after recovery, want 1..1200 once each", len(h.delivered))
	}
	for i, id := range h.delivered {
		if id != int64(i+1) {
			t.Fatalf("delivered[%d] = %d, want %d", i, id, i+1)
		}
	}
}

func TestControlRetryGenerationChangeKeepsNewCursor(t *testing.T) {
	// A lease transition during the failed delivery means the batch belongs
	// to an earlier period: its partial cursor must not replace the cursor
	// that the new period stored.
	h := newCursorHarness(t, 4)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 3 {
			h.c.LeaseLost()
			h.acquire(9, nil)
		}
	}
	failOnce(h, 3)
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 9 {
		t.Fatalf("cursor = %d, want the new LatestID 9", got)
	}
	if want := []int64{1, 2}; !equalIDs(h.delivered, want) {
		t.Fatalf("delivered = %v, want %v", h.delivered, want)
	}
}

func TestControlRetryLeaseLostLeavesCursorUnset(t *testing.T) {
	h := newCursorHarness(t, 3)
	h.acquire(0, nil)
	h.onHandle = func(id int64) {
		if id == 2 {
			h.c.LeaseLost()
		}
	}
	failOnce(h, 2)
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != -1 {
		t.Fatalf("cursor = %d, want -1", got)
	}
}

func TestControlRetryLogsEventAndSandbox(t *testing.T) {
	h := newCursorHarness(t, 2)
	// processEvents runs on this goroutine, so the buffer needs no lock.
	var logs bytes.Buffer
	h.c.S.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
	h.all[1].SandboxID = "sb-retry"
	h.all[1].DeviceID = "dev-retry"
	h.acquire(0, nil)
	failOnce(h, 2)
	h.c.processEvents(context.Background())
	got := logs.String()
	for _, want := range []string{`"msg":"control_event_retry"`, `"event_id":2`, `"sandbox_id":"sb-retry"`, `"device_id":"dev-retry"`, errInjectedDelivery.Error()} {
		if !strings.Contains(got, want) {
			t.Fatalf("logs lack %s:\n%s", want, got)
		}
	}
}

// TestControlHandleDeleteLookup checks which outcomes of the sandbox lookup
// of a delete event are retried and which are skipped.
func TestControlHandleDeleteLookup(t *testing.T) {
	errDB := errors.New("injected database failure")
	for _, tc := range []struct {
		name    string
		offline bool
		row     *sandboxRow
		err     error
		wantErr error
		lookups int
	}{
		{name: "transient error is retried", err: errDB, wantErr: errDB, lookups: 1},
		{name: "missing row is skipped", err: connect.NewError(connect.CodeNotFound, errors.New("sandbox not found")), lookups: 1},
		{name: "terminal sandbox is skipped", row: &sandboxRow{AgentState: ownsandv1.SandboxState_SANDBOX_STATE_DELETED, DeletePending: true}, lookups: 1},
		{name: "failed sandbox is skipped", row: &sandboxRow{AgentState: ownsandv1.SandboxState_SANDBOX_STATE_FAILED, DeletePending: true}, lookups: 1},
		{name: "non-pending delete is skipped", row: &sandboxRow{AgentState: ownsandv1.SandboxState_SANDBOX_STATE_RUNNING}, lookups: 1},
		{name: "offline device is skipped without a lookup", offline: true, err: errDB},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newCursorHarness(t, 0)
			if !tc.offline {
				h.c.S.Hub.Register(context.Background(), "dev-1", "org-1").SetReconciled()
			}
			lookups := 0
			h.c.loadDeleteTarget = func(context.Context, string, string) (*sandboxRow, error) {
				lookups++
				return tc.row, tc.err
			}
			err := h.c.handle(context.Background(), events.Event{ID: 1, Kind: events.KindSandboxDeleteRequested, OrganizationID: "org-1", DeviceID: "dev-1", SandboxID: "sb-1"})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("handle returned a different error than the wanted one (got nil: %t)", err == nil)
			}
			if lookups != tc.lookups {
				t.Fatalf("lookups = %d, want %d", lookups, tc.lookups)
			}
		})
	}
}

// TestControlObsoleteDeleteDoesNotBlockLaterEvents drives processEvents with
// the real handler: a delete event whose sandbox is gone is skipped, and the
// events after it are delivered and stored.
func TestControlObsoleteDeleteDoesNotBlockLaterEvents(t *testing.T) {
	h := newCursorHarness(t, 0)
	h.c.S.Hub.Register(context.Background(), "dev-1", "org-1").SetReconciled()
	h.all = []events.Event{
		{ID: 1, Kind: events.KindSandboxDeleteRequested, OrganizationID: "org-1", DeviceID: "dev-1", SandboxID: "gone"},
		{ID: 2, Kind: events.KindDeviceConfigChanged, DeviceID: "no-channel"},
	}
	h.c.loadDeleteTarget = func(context.Context, string, string) (*sandboxRow, error) {
		return nil, connect.NewError(connect.CodeNotFound, errors.New("sandbox not found"))
	}
	h.c.handleEvent = h.c.handle
	h.acquire(0, nil)
	h.c.processEvents(context.Background())
	if got := h.cursor(); got != 2 {
		t.Fatalf("cursor = %d, want 2", got)
	}
}
