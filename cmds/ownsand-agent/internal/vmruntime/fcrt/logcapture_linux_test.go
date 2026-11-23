// SPDX-License-Identifier: FSL-1.1-ALv2

package fcrt

import (
	"context"
	"log/slog"
	"sync"
	"testing"
)

// guestSyncFailedMessage is the warning the runtime logs when the guest
// flush before a pause does not complete with a zero exit.
const guestSyncFailedMessage = "guest sync before pause failed"

// captureHandler is a slog.Handler that keeps every record in memory. Tests
// match records by level and message, never by their formatted text.
type captureHandler struct {
	mu      sync.Mutex
	records []slog.Record
}

func (h *captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.records = append(h.records, r.Clone())
	return nil
}

// The runtime logger does not add attributes or groups through the handler,
// so a derived handler can share the same record list.
func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// count returns how many records have the level and the message.
func (h *captureHandler) count(level slog.Level, msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.records {
		if r.Level == level && r.Message == msg {
			n++
		}
	}
	return n
}

// newCapturingRuntime returns a runtime that logs into the returned handler.
func newCapturingRuntime(t *testing.T) (*Runtime, *captureHandler) {
	t.Helper()
	h := &captureHandler{}
	r, err := New(Options{Logger: slog.New(h)})
	if err != nil {
		t.Fatal(err)
	}
	return r, h
}

// newFlushCheckedRuntime returns a runtime for a test that expects every
// guest flush to succeed: the test fails at cleanup if the runtime logged
// that a guest sync failed, because Pause only logs that failure and goes on.
func newFlushCheckedRuntime(t *testing.T) *Runtime {
	t.Helper()
	r, h := newCapturingRuntime(t)
	t.Cleanup(func() {
		if n := h.count(slog.LevelWarn, guestSyncFailedMessage); n != 0 {
			t.Errorf("the runtime logged %q %d time(s), want none", guestSyncFailedMessage, n)
		}
	})
	return r
}

// A guest that answers the flush without an exit status makes the runtime log
// the sync warning; this proves the capture in the other tests can fail.
func TestPauseLogsGuestSyncWithoutExitStatus(t *testing.T) {
	r, h := newCapturingRuntime(t)
	rec := &recorder{omitExitStatus: true}
	v := startFakeVM(t, r, "s6", rec)
	if err := r.Pause(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	if n := h.count(slog.LevelWarn, guestSyncFailedMessage); n != 1 {
		t.Fatalf("%d %q warnings, want 1", n, guestSyncFailedMessage)
	}
}
