// SPDX-License-Identifier: FSL-1.1-ALv2

package ratelimit

import (
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
