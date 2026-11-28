// SPDX-License-Identifier: FSL-1.1-ALv2

package billing

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestRecorderSealAndClose checks that the final snapshot contains every
// count added before it and that a count added after it is dropped with a
// log record, not kept in memory.
func TestRecorderSealAndClose(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	r := NewRecorder(slog.New(slog.NewJSONHandler(&logs, nil)))
	org := "0190f3a4-0000-7000-8000-000000000001"
	at := time.Date(2026, 11, 21, 10, 2, 0, 0, time.UTC)
	r.Add(org, classA, at, 2)
	if err := r.sealBatch(true, at); err != nil {
		t.Fatal(err)
	}
	batch := r.firstPending()
	counts := batch.counts
	if got := counts[counterKey{org, classA, WindowStart(at).Unix()}]; got != 2 {
		t.Fatalf("final snapshot = %v, want 2 for the counter", counts)
	}
	logs.Reset()
	r.Add(org, classA, at, 7)
	if n := r.Pending(); n != 1 {
		t.Fatalf("pending after close = %d, want 1 (only the final batch)", n)
	}
	type lateUsageLog struct {
		Message        string `json:"msg"`
		OrganizationID string `json:"organization_id"`
		Meter          string `json:"meter"`
		WindowStart    string `json:"window_start"`
		Quantity       int64  `json:"quantity"`
	}
	decoder := json.NewDecoder(&logs)
	var got lateUsageLog
	if err := decoder.Decode(&got); err != nil {
		t.Fatalf("decode first late usage log: %v", err)
	}
	var extra json.RawMessage
	if err := decoder.Decode(&extra); err != io.EOF {
		t.Fatalf("expected exactly one late usage log record, got extra data %q or error %v", extra, err)
	}
	want := lateUsageLog{
		Message:        "billing_usage_dropped_after_final_flush",
		OrganizationID: org,
		Meter:          "ownsand.class_a_rpc",
		WindowStart:    "2026-11-21T10:00:00Z",
		Quantity:       7,
	}
	if got != want {
		t.Errorf("late usage log = %+v, want %+v", got, want)
	}
	// A failed final flush keeps its batch; the recorder stays closed.
	r.Add(org, classA, at, 1)
	if got := r.firstPending(); got != batch || got.counts[counterKey{org, classA, WindowStart(at).Unix()}] != 2 {
		t.Errorf("pending batch = %+v, want the final batch with 2", got)
	}
	if n := r.Pending(); n != 1 {
		t.Errorf("pending after a late add = %d, want 1", n)
	}
}
