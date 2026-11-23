// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"testing"
	"time"
)

// TestCommandRetentionNowUsesElapsedSource checks that the retention reading
// follows CommandElapsed and not the wall clock, and that its default is the
// process monotonic clock.
func TestCommandRetentionNowUsesElapsedSource(t *testing.T) {
	wall := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var elapsed time.Duration
	s := &Service{Now: func() time.Time { return wall }, CommandElapsed: func() time.Duration { return elapsed }}

	first := s.commandRetentionNow()
	if first.IsZero() {
		t.Fatal("a reading equals the zero time that marks an open channel")
	}
	wall = wall.Add(-time.Hour)
	if got := s.commandRetentionNow(); !got.Equal(first) {
		t.Fatalf("a wall step changed the reading: %v -> %v", first, got)
	}
	elapsed += 90 * time.Second
	if got := s.commandRetentionNow().Sub(first); got != 90*time.Second {
		t.Fatalf("elapsed time moved the reading by %v, want 90s", got)
	}

	d := &Service{Now: func() time.Time { return wall }}
	a := d.commandRetentionNow()
	time.Sleep(5 * time.Millisecond)
	if got := d.commandRetentionNow().Sub(a); got <= 0 {
		t.Fatalf("the default source did not advance: %v", got)
	}
}
