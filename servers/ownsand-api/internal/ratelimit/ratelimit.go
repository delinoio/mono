// SPDX-License-Identifier: FSL-1.1-ALv2

// Package ratelimit implements in-memory token buckets keyed by string (a
// device ID, a credential display prefix, a principal or a source address).
package ratelimit

import (
	"sync"
	"time"
)

// Limiter is a set of token buckets with the same rate and burst.
type Limiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	sweep   time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New returns a limiter that allows burst events at once and then rate events
// per second for each key.
func New(rate float64, burst int, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{rate: rate, burst: float64(burst), now: now, buckets: map[string]*bucket{}}
}

func (l *Limiter) refill(key string, now time.Time) *bucket {
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = min(l.burst, b.tokens+elapsed*l.rate)
		b.last = now
	}
	// Drop full buckets once a minute so that idle keys do not accumulate.
	if now.Sub(l.sweep) > time.Minute {
		l.sweep = now
		for k, other := range l.buckets {
			if k != key && other.tokens+now.Sub(other.last).Seconds()*l.rate >= l.burst {
				delete(l.buckets, k)
			}
		}
	}
	return b
}

// Allow takes one token for key and reports whether one was available.
func (l *Limiter) Allow(key string) bool {
	ok, _ := l.Reserve(key, 1)
	return ok
}

// Reserve takes n tokens for key. When they are not available it takes none
// and returns the delay after which they will be.
func (l *Limiter) Reserve(key string, n float64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.refill(key, l.now())
	if b.tokens >= n {
		b.tokens -= n
		return true, 0
	}
	missing := n - b.tokens
	if l.rate <= 0 {
		return false, time.Hour
	}
	return false, time.Duration(missing / l.rate * float64(time.Second))
}

// Peek reports whether key has at least one token, without taking it.
func (l *Limiter) Peek(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.refill(key, l.now()).tokens >= 1
}
