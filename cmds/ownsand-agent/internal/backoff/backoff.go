// SPDX-License-Identifier: Apache-2.0

// Package backoff computes jittered exponential reconnect delays.
package backoff

import (
	"context"
	"math/rand/v2"
	"time"
)

// Backoff doubles the delay after each failure up to Max. It is not safe
// for concurrent use.
type Backoff struct {
	Min, Max time.Duration
	cur      time.Duration
}

// Next returns the next delay: the current step with up to 20% jitter
// below it, never more than Max.
func (b *Backoff) Next() time.Duration {
	if b.cur == 0 {
		b.cur = b.Min
	} else {
		b.cur = min(b.cur*2, b.Max)
	}
	jitter := time.Duration(rand.Int64N(int64(b.cur)/5 + 1))
	return b.cur - jitter
}

// Reset starts again from Min.
func (b *Backoff) Reset() { b.cur = 0 }

// Sleep waits d or until ctx ends.
func Sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
