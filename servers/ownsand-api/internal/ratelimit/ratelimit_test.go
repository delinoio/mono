// SPDX-License-Identifier: FSL-1.1-ALv2

package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(10, 10, func() time.Time { return now })
	for i := 0; i < 10; i++ {
		if !l.Allow("p") {
			t.Fatalf("event %d denied", i)
		}
	}
	ok, delay := l.Reserve("p", 1)
	if ok || delay != 100*time.Millisecond {
		t.Fatalf("over the limit: %v %v", ok, delay)
	}
	if !l.Allow("q") {
		t.Fatal("keys share buckets")
	}
	now = now.Add(100 * time.Millisecond)
	if !l.Allow("p") {
		t.Fatal("no refill")
	}
	if l.Peek("p") {
		t.Fatal("peek after the refilled token was used")
	}
}
