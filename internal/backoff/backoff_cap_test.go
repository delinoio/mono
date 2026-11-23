// SPDX-License-Identifier: Apache-2.0

package backoff

import (
	"testing"
	"time"
)

// TestBackoffMaxBelowMinCapsEveryStep checks that Max also bounds the first
// step and the first step after Reset when Max is below Min.
func TestBackoffMaxBelowMinCapsEveryStep(t *testing.T) {
	b := Backoff{Min: 200 * time.Millisecond, Max: 100 * time.Millisecond}
	check := func(label string) {
		t.Helper()
		for i := range 50 {
			d := b.Next()
			if d < 80*time.Millisecond || d > 100*time.Millisecond {
				t.Fatalf("%s step %d delay %v outside 80ms-100ms", label, i, d)
			}
		}
	}
	check("initial")
	b.Reset()
	check("after reset")
}

// TestBackoffStandardRelayTiming checks the relay timing
// (Min 200ms, Max 5s) still starts at 160ms-200ms and grows to the cap.
func TestBackoffStandardRelayTiming(t *testing.T) {
	b := Backoff{Min: 200 * time.Millisecond, Max: 5 * time.Second}
	if d := b.Next(); d < 160*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("first delay %v outside 160ms-200ms", d)
	}
	longest := time.Duration(0)
	for i := range 20 {
		d := b.Next()
		if d > 5*time.Second {
			t.Fatalf("step %d delay %v exceeds the cap", i, d)
		}
		longest = max(longest, d)
	}
	if longest < 4*time.Second {
		t.Fatalf("delay never reached the cap range: %v", longest)
	}
	b.Reset()
	if d := b.Next(); d < 160*time.Millisecond || d > 200*time.Millisecond {
		t.Fatalf("delay after reset %v outside 160ms-200ms", d)
	}
}
