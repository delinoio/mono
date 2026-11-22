// SPDX-License-Identifier: Apache-2.0

package ownsand

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeElapsed is a controlled monotonic clock for an activity, so the tests
// below depend on no sleep and no timer resolution.
type fakeElapsed struct{ ns atomic.Int64 }

func (f *fakeElapsed) advance(d time.Duration) { f.ns.Add(int64(d)) }
func (f *fakeElapsed) elapsed() time.Duration  { return time.Duration(f.ns.Load()) }

func newFakeActivity(f *fakeElapsed) *activity {
	a := &activity{base: time.Now(), elapsed: f.elapsed}
	a.touch()
	return a
}

func TestActivityIdleGrowsWithElapsedTimeAndTouchResetsIt(t *testing.T) {
	var f fakeElapsed
	a := newFakeActivity(&f)
	if got := a.idle(); got != 0 {
		t.Fatalf("idle of a new activity = %v, want 0", got)
	}
	f.advance(89 * time.Second)
	if got := a.idle(); got != 89*time.Second {
		t.Fatalf("idle = %v, want 89s", got)
	}
	a.touch()
	if got := a.idle(); got != 0 {
		t.Fatalf("idle after touch = %v, want 0", got)
	}
	f.advance(90 * time.Second)
	if got := a.idle(); got < defaultTiming().streamStall {
		t.Fatalf("idle = %v, want at least the stall timeout %v", got, defaultTiming().streamStall)
	}
}

func TestActivityIdleNeverNegative(t *testing.T) {
	var f fakeElapsed
	f.advance(time.Minute)
	a := newFakeActivity(&f)
	// A clock that moves back must not report a negative idle time.
	f.advance(-10 * time.Second)
	if got := a.idle(); got != 0 {
		t.Fatalf("idle = %v, want 0", got)
	}
}

// TestActivityBaseKeepsMonotonicReading guards the property that makes the
// idle time immune to a wall-clock step: time.Since uses the monotonic
// reading of its argument. Round(0), a Unix-time round trip or a Local/UTC
// conversion that strips it would make idle() wall-clock based again, and
// the String form of a Time shows the reading as "m=".
func TestActivityBaseKeepsMonotonicReading(t *testing.T) {
	a := newActivity()
	if !strings.Contains(a.base.String(), " m=") {
		t.Fatalf("activity base %q has no monotonic clock reading", a.base.String())
	}
	if stripped := time.Unix(0, a.base.UnixNano()); strings.Contains(stripped.String(), " m=") {
		t.Fatalf("control: a Unix-time rebuild %q unexpectedly keeps a monotonic reading", stripped.String())
	}
	// With the real clock the idle time is small and a touch never raises it.
	if got := a.idle(); got < 0 || got > time.Minute {
		t.Fatalf("idle of a new activity = %v", got)
	}
}

func TestActivityBodyReadResetsIdle(t *testing.T) {
	var f fakeElapsed
	a := newFakeActivity(&f)
	body := &activityBody{ReadCloser: io.NopCloser(strings.NewReader("abc")), a: a}

	f.advance(60 * time.Second)
	if got := a.idle(); got != 60*time.Second {
		t.Fatalf("idle = %v, want 60s", got)
	}
	buf := make([]byte, 2)
	if n, err := body.Read(buf); n != 2 || err != nil {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if got := a.idle(); got != 0 {
		t.Fatalf("idle after a byte = %v, want 0", got)
	}

	// A read that returns no byte (here the end of the body) does not reset it.
	f.advance(30 * time.Second)
	buf = make([]byte, 8)
	if n, _ := body.Read(buf); n != 1 {
		t.Fatalf("Read = %d, want 1", n)
	}
	f.advance(10 * time.Second)
	if n, err := body.Read(buf); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %d, %v, want 0, EOF", n, err)
	}
	if got := a.idle(); got != 10*time.Second {
		t.Fatalf("idle after an empty read = %v, want 10s", got)
	}
}

// TestStallContextByteBeforeExpiryResetsIdle runs the real stall detector with
// a scaled stall timeout. The bytes arrive for twice the stall timeout, which
// a detector that does not reset on a byte would cancel, and the stream is
// cancelled with errStall only after the bytes stop. Every bound is generous,
// so a slow or coarse-timer host passes.
func TestStallContextByteBeforeExpiryResetsIdle(t *testing.T) {
	c := &Client{timing: testTiming()}
	c.timing.streamStall = 600 * time.Millisecond

	actx, _, stop := c.stallContext(context.Background(), 0)
	defer stop()
	a, ok := actx.Value(activityKey{}).(*activity)
	if !ok {
		t.Fatal("stall context carries no activity")
	}

	deadline := time.Now().Add(2 * c.timing.streamStall)
	for time.Now().Before(deadline) {
		a.touch()
		if err := context.Cause(actx); err != nil {
			t.Fatalf("stream cancelled with %v while bytes still arrive", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	select {
	case <-actx.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("stream not cancelled after the bytes stopped")
	}
	if cause := context.Cause(actx); !errors.Is(cause, errStall) {
		t.Fatalf("cause = %v, want errStall", cause)
	}
}
