// SPDX-License-Identifier: Apache-2.0

package backoff

import (
	"testing"
	"time"
)

func TestBackoffBounds(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 5 * time.Second}
	prevMax := time.Duration(0)
	for i := range 10 {
		d := b.Next()
		if d > 5*time.Second || d < 800*time.Millisecond {
			t.Fatalf("step %d delay %v", i, d)
		}
		prevMax = max(prevMax, d)
	}
	if prevMax < 4*time.Second {
		t.Fatalf("delay never grew: %v", prevMax)
	}
	b.Reset()
	if d := b.Next(); d > time.Second {
		t.Fatalf("after reset %v", d)
	}
}
