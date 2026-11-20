// Package clock is the injectable time source of the OwnSand Go SDK.
//
// The SDK measures relay grant lifetimes, recovery windows, stream idle
// timeouts, WriteFile attempt deadlines and the peer-to-peer open timeout on
// a Clock. Production code uses Real. Tests use Scaled, which runs virtual
// time faster than real time and can jump ahead, so that rules measured in
// minutes run in milliseconds.
package clock

import (
	"context"
	"sync"
	"time"
)

// Clock is a monotonic time source.
type Clock interface {
	// Now returns the current time. Only differences between two Now values
	// are meaningful; the SDK never compares Now with server timestamps.
	Now() time.Time
	// NewTimer returns a timer that fires once after d.
	NewTimer(d time.Duration) Timer
}

// Timer is a one-shot timer.
type Timer interface {
	// C returns the channel that receives the fire time.
	C() <-chan time.Time
	// Stop prevents the timer from firing. It reports whether it stopped
	// the timer before it fired.
	Stop() bool
}

// Real returns the clock of the process. time.Now carries a monotonic
// reading, so differences between Now values do not change when the wall
// clock is set.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTimer(d time.Duration) Timer { return realTimer{time.NewTimer(d)} }

type realTimer struct{ t *time.Timer }

func (t realTimer) C() <-chan time.Time { return t.t.C }
func (t realTimer) Stop() bool          { return t.t.Stop() }

// Sleep waits for d on c. It returns the context error when ctx ends first.
func Sleep(ctx context.Context, c Clock, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := c.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C():
		return nil
	}
}

// WithTimeout returns a context that ends with cause when d passes on c.
func WithTimeout(ctx context.Context, c Clock, d time.Duration, cause error) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(ctx)
	t := c.NewTimer(d)
	go func() {
		select {
		case <-t.C():
			cancel(cause)
		case <-ctx.Done():
			t.Stop()
		}
	}()
	return ctx, func() { cancel(context.Canceled) }
}

// Scaled is a test clock whose virtual time runs Scale times faster than
// real time and that Advance moves forward at once.
type Scaled struct {
	mu        sync.Mutex
	realStart time.Time
	virtStart time.Time
	scale     float64
	offset    time.Duration
	// advanced is closed and replaced at every Advance, so timer goroutines
	// recompute their real wait.
	advanced chan struct{}
}

// NewScaled returns a Scaled clock that starts at the current time.
func NewScaled(scale float64) *Scaled {
	if scale <= 0 {
		scale = 1
	}
	now := time.Now()
	return &Scaled{realStart: now, virtStart: now, scale: scale, advanced: make(chan struct{})}
}

// Now returns the virtual time.
func (s *Scaled) Now() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.nowLocked()
}

func (s *Scaled) nowLocked() time.Time {
	elapsed := time.Duration(float64(time.Since(s.realStart)) * s.scale)
	return s.virtStart.Add(elapsed + s.offset)
}

// Advance moves virtual time forward by d and fires every timer that is due.
func (s *Scaled) Advance(d time.Duration) {
	s.mu.Lock()
	s.offset += d
	close(s.advanced)
	s.advanced = make(chan struct{})
	s.mu.Unlock()
}

// Real converts a virtual duration to the real duration it takes.
func (s *Scaled) Real(d time.Duration) time.Duration {
	return time.Duration(float64(d) / s.scale)
}

// NewTimer returns a timer on virtual time.
func (s *Scaled) NewTimer(d time.Duration) Timer {
	s.mu.Lock()
	deadline := s.nowLocked().Add(d)
	s.mu.Unlock()
	t := &scaledTimer{c: make(chan time.Time, 1), stop: make(chan struct{})}
	go t.run(s, deadline)
	return t
}

type scaledTimer struct {
	c        chan time.Time
	stop     chan struct{}
	stopOnce sync.Once
	mu       sync.Mutex
	fired    bool
}

func (t *scaledTimer) run(s *Scaled, deadline time.Time) {
	for {
		s.mu.Lock()
		now := s.nowLocked()
		adv := s.advanced
		s.mu.Unlock()
		remaining := deadline.Sub(now)
		if remaining <= 0 {
			t.mu.Lock()
			if !t.fired {
				select {
				case <-t.stop:
				default:
					t.fired = true
					t.c <- now
				}
			}
			t.mu.Unlock()
			return
		}
		rt := time.NewTimer(max(s.Real(remaining), time.Microsecond))
		select {
		case <-rt.C:
		case <-adv:
			rt.Stop()
		case <-t.stop:
			rt.Stop()
			return
		}
	}
}

func (t *scaledTimer) C() <-chan time.Time { return t.c }

func (t *scaledTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopOnce.Do(func() { close(t.stop) })
	return !t.fired
}
