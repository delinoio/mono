// SPDX-License-Identifier: FSL-1.1-ALv2

package ratelimit

import (
	"fmt"
	"testing"
	"time"
)

func TestLimiter(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(1, 2, func() time.Time { return now })
	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow("a", 1); !ok {
			t.Fatalf("request %d rejected", i)
		}
	}
	ok, retry := l.Allow("a", 1)
	if ok || retry != time.Second {
		t.Fatalf("third request: %v %v", ok, retry)
	}
	if ok, _ := l.Allow("b", 1); !ok {
		t.Fatal("keys share a bucket")
	}
	if ok, _ := l.Peek("a"); ok {
		t.Fatal("peek on an empty bucket")
	}
	now = now.Add(time.Second)
	if ok, _ := l.Peek("a"); !ok {
		t.Fatal("peek after refill")
	}
	if ok, _ := l.Allow("a", 1); !ok {
		t.Fatal("refill not applied")
	}
	if ok, _ := l.Peek("never-seen"); !ok {
		t.Fatal("peek on a new key")
	}
}

// fillLimiter charges keys distinct keys, one per millisecond, so that key
// i has a strictly older last-charge time than key i+1.
func fillLimiter(l *Limiter, now *time.Time, keys int, cost float64) {
	for i := 0; i < keys; i++ {
		if ok, _ := l.Allow(fmt.Sprintf("k%d", i), cost); !ok {
			panic("fill request rejected")
		}
		*now = now.Add(time.Millisecond)
	}
}

func TestEvictDropsOldestHalf(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(0.001, 2, func() time.Time { return now })
	fillLimiter(l, &now, maxKeys, 2)
	if ok, _ := l.Allow("new", 1); !ok {
		t.Fatal("new key rejected")
	}
	if got, want := len(l.buckets), maxKeys-maxKeys/2+1; got != want {
		t.Fatalf("buckets after eviction: %d, want %d", got, want)
	}
	for i := 0; i < maxKeys; i++ {
		key := fmt.Sprintf("k%d", i)
		_, kept := l.buckets[key]
		if want := i >= maxKeys/2; kept != want {
			t.Fatalf("key %s kept=%v, want %v", key, kept, want)
		}
	}
	if ok, _ := l.Peek(fmt.Sprintf("k%d", maxKeys-1)); ok {
		t.Fatal("newest bucket lost its limit")
	}
	if ok, _ := l.Peek("k0"); !ok {
		t.Fatal("evicted bucket still limited")
	}
}

func TestEvictDropsFullAgainBucketsFirst(t *testing.T) {
	now := time.Unix(0, 0)
	l := New(0.001, 2, func() time.Time { return now })
	// The lightly charged key refills (1000s) before the others (2000s).
	if ok, _ := l.Allow("light", 1); !ok {
		t.Fatal("light rejected")
	}
	fillLimiter(l, &now, maxKeys-1, 2)
	now = now.Add(1500 * time.Second)
	if ok, _ := l.Allow("new", 1); !ok {
		t.Fatal("new key rejected")
	}
	if _, ok := l.buckets["light"]; ok {
		t.Fatal("refilled bucket kept")
	}
	if got, want := len(l.buckets), maxKeys; got != want {
		t.Fatalf("buckets: %d, want %d", got, want)
	}
	if ok, _ := l.Allow("k0", 2); ok {
		t.Fatal("oldest charged bucket was evicted although a refilled one was reclaimed")
	}
}
