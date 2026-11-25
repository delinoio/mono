// SPDX-License-Identifier: FSL-1.1-ALv2

package relay

import "time"

// Clock supplies the wall and monotonic readings for a relay connection.
// Idle, keepalive and acceptance deadlines use elapsed time. LastReceive
// and LastLargeReceive report wall timestamps.
type Clock interface {
	Now() time.Time
	Elapsed() time.Duration
}

// systemClock keeps the monotonic reading that time.Now captured, so
// time.Since measures elapsed monotonic time.
type systemClock struct{ base time.Time }

func newSystemClock() systemClock { return systemClock{base: time.Now()} }

func (systemClock) Now() time.Time { return time.Now() }

func (s systemClock) Elapsed() time.Duration { return time.Since(s.base) }
