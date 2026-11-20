// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"sync"
	"time"
)

// timing holds the durations of the data path rules. Tests scale them down;
// production uses defaultTiming, which follows the SDK contract.
type timing struct {
	// p2pOpenTimeout bounds the wait for the data channel to open.
	p2pOpenTimeout time.Duration
	// recoveryWindow is the relay recovery window and the retry window of
	// AuthorizeRelay outside recovery.
	recoveryWindow time.Duration
	// streamStall cancels a relay server stream that receives no byte.
	streamStall time.Duration
	// relayStreamLimit plus streamMargin is the lifetime a grant needs for
	// a new server stream.
	relayStreamLimit time.Duration
	// writeCallLimit plus streamMargin is the lifetime a grant needs for a
	// WriteFile chunk.
	writeCallLimit time.Duration
	streamMargin   time.Duration
	// grantIdle is how long after the last stream of a grant ended the
	// agent may have released it; the SDK renews it after that.
	grantIdle time.Duration
	// writeAttemptDeadline is the SDK-side deadline of one WriteFile attempt.
	writeAttemptDeadline time.Duration
	// backoffMin and backoffMax bound the retry back-off.
	backoffMin time.Duration
	backoffMax time.Duration
	// pollInterval is the interval of GetSandbox polling while the SDK
	// waits for a stable state or for a paused sandbox to resume.
	pollInterval time.Duration
	// createRetryWindow bounds the retries of a lost CreateSandbox response.
	createRetryWindow time.Duration
}

func defaultTiming() timing {
	return timing{
		p2pOpenTimeout:       10 * time.Second,
		recoveryWindow:       60 * time.Second,
		streamStall:          90 * time.Second,
		relayStreamLimit:     14 * time.Minute,
		writeCallLimit:       5 * time.Minute,
		streamMargin:         30 * time.Second,
		grantIdle:            60 * time.Second,
		writeAttemptDeadline: 5*time.Minute + 30*time.Second,
		backoffMin:           250 * time.Millisecond,
		backoffMax:           5 * time.Second,
		pollInterval:         2 * time.Second,
		createRetryWindow:    60 * time.Second,
	}
}

// recovery is the 60-second relay recovery window of one operation. A
// failure starts it; an accepted continuation pauses its clock while the
// stream stays open; data, a terminal result, a clean rotation of an
// accepted continuation or a paused event ends it.
type recovery struct {
	mu       sync.Mutex
	window   time.Duration
	active   bool
	paused   bool
	used     time.Duration // time used before the current running period
	running  time.Time     // start of the current running period
	lastErr  error
	attempts int
}

func newRecovery(window time.Duration) *recovery {
	return &recovery{window: window}
}

// fail records a failure and starts the window when it is not active. A
// paused clock resumes.
func (r *recovery) fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lastErr = err
	r.attempts++
	if !r.active {
		r.active = true
		r.paused = false
		r.used = 0
		r.running = time.Now()
		return
	}
	if r.paused {
		r.paused = false
		r.running = time.Now()
	}
}

// pauseClock stops the clock while an accepted continuation stays open.
func (r *recovery) pauseClock() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active && !r.paused {
		r.used += time.Since(r.running)
		r.paused = true
	}
}

// reset ends the window.
func (r *recovery) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.active = false
	r.paused = false
	r.used = 0
	r.lastErr = nil
	r.attempts = 0
}

// inRecovery reports whether the window is active.
func (r *recovery) inRecovery() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.active
}

// remaining returns the time left in an active window.
func (r *recovery) remaining() time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.active {
		return r.window
	}
	used := r.used
	if !r.paused {
		used += time.Since(r.running)
	}
	return r.window - used
}

// backoff waits before the next attempt: at least minDelay, with an
// exponential back-off. It returns the last failure when the window ends
// first, or the context error.
func (r *recovery) backoff(ctx context.Context, t timing, minDelay time.Duration) error {
	r.mu.Lock()
	n := r.attempts
	lastErr := r.lastErr
	r.mu.Unlock()
	delay := t.backoffMin
	for i := 1; i < n && delay < t.backoffMax; i++ {
		delay *= 2
	}
	delay = min(delay, t.backoffMax)
	delay = max(delay, minDelay)
	left := r.remaining()
	if left <= 0 {
		return lastErr
	}
	if delay >= left {
		// Waiting the full delay would pass the window. Wait it out and
		// end with the last error, so the operation ends within the window.
		if err := sleep(ctx, left); err != nil {
			return err
		}
		return lastErr
	}
	return sleep(ctx, delay)
}

// expired reports whether an active window has ended.
func (r *recovery) expired() bool {
	return r.inRecovery() && r.remaining() <= 0
}

func (r *recovery) err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lastErr
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
