// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import "time"

// connClock supplies the two readings that a relay connection needs. The idle
// and keepalive thresholds use mono, an elapsed monotonic reading, so a step
// of the wall clock (an NTP correction, a manual change, a VM resume) neither
// closes a healthy connection early nor keeps a dead one open longer. The
// observers LastReceive and LastLargeReceive report wall, a wall timestamp
// for callers that compare it with other wall times.
type connClock interface {
	// wall returns the current wall time.
	wall() time.Time
	// mono returns the monotonic time that has elapsed since the clock was
	// created.
	mono() time.Duration
}

// systemClock is the production connClock. base keeps the monotonic reading
// that time.Now captured, so time.Since measures elapsed monotonic time.
type systemClock struct{ base time.Time }

func newSystemClock() systemClock { return systemClock{base: time.Now()} }

func (systemClock) wall() time.Time { return time.Now() }

func (s systemClock) mono() time.Duration { return time.Since(s.base) }
