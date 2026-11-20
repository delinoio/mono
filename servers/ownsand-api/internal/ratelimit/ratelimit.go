// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ratelimit holds in-memory token bucket limiters keyed by string.
// The control plane runs as one replica (see the Runtime and Language
// section of docs/servers-ownsand-api-foundation.md), so in-memory state is
// the limit of the whole service.
package ratelimit

import (
	"math"
	"sync"
	"time"
)

// maxKeys bounds the number of tracked keys. When it is reached, buckets
// that are full again are dropped; if none is, the oldest half is dropped,
// which forgets some limits instead of growing without bound.
const maxKeys = 100_000

// Limiter is a token bucket per key: rate tokens per second up to burst.
type Limiter struct {
	rate  float64
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter that allows burst requests at once and rate
// requests per second on average for each key.
func New(rate float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

// Allow takes cost tokens from key's bucket. It reports whether the request
// is allowed and, when it is not, how long until it would be.
func (l *Limiter) Allow(key string, cost float64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxKeys {
			l.evictLocked(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens >= cost {
		b.tokens -= cost
		return true, 0
	}
	missing := cost - b.tokens
	return false, time.Duration(math.Ceil(missing/l.rate*1000)) * time.Millisecond
}

// Peek reports whether key has at least one token, without taking it. It
// is used for limits that count only failures: the caller peeks before the
// check and takes a token with Allow only when the check fails.
func (l *Limiter) Peek(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return true, 0
	}
	tokens := math.Min(l.burst, b.tokens+l.now().Sub(b.last).Seconds()*l.rate)
	if tokens >= 1 {
		return true, 0
	}
	return false, time.Duration(math.Ceil((1-tokens)/l.rate*1000)) * time.Millisecond
}

func (l *Limiter) evictLocked(now time.Time) {
	for key, b := range l.buckets {
		if b.tokens+now.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.buckets, key)
		}
	}
	if len(l.buckets) < maxKeys {
		return
	}
	n := 0
	for key := range l.buckets {
		delete(l.buckets, key)
		n++
		if n >= maxKeys/2 {
			return
		}
	}
}
