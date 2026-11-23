// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// TestRecorderTakeAndClose checks that the final snapshot contains every
// count added before it and that a count added after it is dropped with a
// log record, not kept in memory.
func TestRecorderTakeAndClose(t *testing.T) {
	var logs bytes.Buffer
	r := NewRecorder(slog.New(slog.NewJSONHandler(&logs, nil)))
	org := "0190f3a4-0000-7000-8000-000000000001"
	at := time.Date(2026, 11, 21, 10, 2, 0, 0, time.UTC)
	r.Add(org, classA, at, 2)
	counts := r.takeAndClose()
	if got := counts[counterKey{org, classA, WindowStart(at).Unix()}]; got != 2 {
		t.Fatalf("final snapshot = %v, want 2 for the counter", counts)
	}
	r.Add(org, classA, at, 7)
	if n := r.Pending(); n != 0 {
		t.Fatalf("pending after close = %d, want 0", n)
	}
	out := logs.String()
	for _, want := range []string{"billing_usage_dropped_after_final_flush", org, `"quantity":7`} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q does not contain %q", out, want)
		}
	}
	// A failed final flush restores its counts; the recorder stays closed.
	r.restore(counts)
	r.Add(org, classA, at, 1)
	if got := r.take()[counterKey{org, classA, WindowStart(at).Unix()}]; got != 2 {
		t.Errorf("restored counter = %d, want 2", got)
	}
}
