// SPDX-License-Identifier: FSL-1.1-ALv2

package dataplane

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestSessionLimitsDefaults(t *testing.T) {
	got := SessionLimits{}.withDefaults()
	if got != (SessionLimits{MaxStreamsPerSession: 64, MaxSessions: 256}) {
		t.Fatalf("defaults %+v", got)
	}
	set := SessionLimits{MaxStreamsPerSession: 3, MaxSessions: 5}.withDefaults()
	if set != (SessionLimits{MaxStreamsPerSession: 3, MaxSessions: 5}) {
		t.Fatalf("explicit limits changed: %+v", set)
	}
	if got := New(Options{}).limits; got != DefaultSessionLimits() {
		t.Fatalf("data plane limits %+v", got)
	}
}

// The rejection log writes the first record at once, then at most one record
// per interval and kind, and carries the count of the suppressed rejections.
func TestLimitRejectLogRateLimit(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))
	var l limitRejectLog
	start := time.Unix(1_700_000_000, 0)
	if !l.note(logger, start, "stream", "s1", "b1", 64) {
		t.Fatal("first rejection not logged")
	}
	if l.note(logger, start.Add(limitRejectLogInterval-time.Nanosecond), "stream", "s1", "b1", 64) {
		t.Fatal("rejection inside the interval logged")
	}
	if l.note(logger, start.Add(time.Second), "stream", "s2", "b1", 64) {
		t.Fatal("rejection inside the interval logged")
	}
	if !l.note(logger, start.Add(limitRejectLogInterval), "stream", "s3", "b1", 64) {
		t.Fatal("rejection after the interval not logged")
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("%d records, want 2: %s", len(lines), buf.String())
	}
	for _, want := range []string{`"session_id":"s3"`, `"sandbox_id":"b1"`, `"limit":64`, `"rejected":3`, `"kind":"stream"`} {
		if !strings.Contains(lines[1], want) {
			t.Fatalf("record %s lacks %s", lines[1], want)
		}
	}
}
