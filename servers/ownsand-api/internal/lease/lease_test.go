// SPDX-License-Identifier: FSL-1.1-ALv2

package lease

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

// testClock is a settable clock for the lease timestamps and deadline.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock {
	return &testClock{now: time.Now().UTC().Truncate(time.Millisecond)}
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func newTestLease() (*Lease, *atomic.Int32) {
	var lost atomic.Int32
	l := &Lease{
		InstanceID: "test",
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnLose:     func() { lost.Add(1) },
	}
	return l, &lost
}

// acquireFor gives the lease to l until the real time expireAt, as a
// confirmed acquisition does.
func acquireFor(t *testing.T, l *Lease, expireAt time.Time) {
	t.Helper()
	epoch, _, _ := l.snapshot()
	l.applyRenewal(epoch, true, "", expireAt)
	if !l.Held() {
		t.Fatal("the acquisition did not give the lease")
	}
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestWhileHeldRunsOnlyWhileHeld checks that fn is not run without the lease.
func TestWhileHeldRunsOnlyWhileHeld(t *testing.T) {
	l, _ := newTestLease()
	ran := false
	if l.WhileHeld(func() { ran = true }) || ran {
		t.Fatal("fn ran without the lease")
	}
	acquireFor(t, l, time.Now().Add(time.Hour))
	if !l.WhileHeld(func() { ran = true }) || !ran {
		t.Fatal("fn did not run with the lease")
	}
	l.lose(reasonReleased)
	ran = false
	if l.WhileHeld(func() { ran = true }) || ran {
		t.Fatal("fn ran after the lease was lost")
	}
}

// TestWhileHeldSerializesWithLoss checks that the loss transition waits for
// a running fn and runs OnLose only after it, so OnLose sees the effects of
// fn, and that a call after the transition does not run fn.
func TestWhileHeldSerializesWithLoss(t *testing.T) {
	l, lost := newTestLease()
	acquireFor(t, l, time.Now().Add(time.Hour))

	inFn := make(chan struct{})
	releaseFn := make(chan struct{})
	whileHeldDone := make(chan bool, 1)
	go func() {
		whileHeldDone <- l.WhileHeld(func() {
			close(inFn)
			<-releaseFn
		})
	}()
	<-inFn

	setDone := make(chan struct{})
	go func() {
		defer close(setDone)
		l.lose(reasonReleased)
	}()
	// The loss transition is blocked on the ownership lock held by fn.
	select {
	case <-setDone:
		t.Fatal("the loss transition did not wait for the running fn")
	case <-time.After(100 * time.Millisecond):
	}
	if got := lost.Load(); got != 0 {
		t.Fatalf("OnLose ran %d times before fn returned", got)
	}

	close(releaseFn)
	if !<-whileHeldDone {
		t.Fatal("WhileHeld reported that fn did not run")
	}
	select {
	case <-setDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the loss transition stayed blocked")
	}
	if got := lost.Load(); got != 1 {
		t.Fatalf("OnLose ran %d times, want 1", got)
	}
	if l.WhileHeld(func() { t.Error("fn ran after the loss transition") }) {
		t.Fatal("WhileHeld ran fn after the loss transition")
	}
}

// TestHeldEndsAtConfirmedExpiry checks that ownership ends at the confirmed
// expiry without the watcher or a renewal result, and that a result that
// arrives after the expiry neither extends nor restores it.
func TestHeldEndsAtConfirmedExpiry(t *testing.T) {
	clock := newTestClock()
	l, lost := newTestLease()
	l.DeadlineNow = clock.Now
	acquireFor(t, l, clock.Now().Add(TTL))
	epoch, expireAt, _ := l.snapshot()

	clock.Advance(TTL - time.Millisecond)
	if !l.Held() || !l.WhileHeld(func() {}) {
		t.Fatal("ownership ended before the expiry")
	}
	clock.Advance(time.Millisecond)
	if l.Held() || l.WhileHeld(func() { t.Error("fn ran at the expiry") }) {
		t.Fatal("ownership continued at the expiry")
	}

	// The watcher has not run: a renewal result of the same period arrives
	// after the expiry. It ends the period and does not extend it.
	l.applyRenewal(epoch, true, "", expireAt.Add(TTL))
	if l.Held() {
		t.Fatal("a result after the expiry extended the ownership")
	}
	if got := lost.Load(); got != 1 {
		t.Fatalf("OnLose ran %d times, want 1", got)
	}
	// The result of the ended period cannot restore it, and a second
	// expiry check does not run OnLose again.
	l.applyRenewal(epoch, true, "", clock.Now().Add(TTL))
	l.expireIfDue()
	if l.Held() || lost.Load() != 1 {
		t.Fatalf("held %v, OnLose ran %d times", l.Held(), lost.Load())
	}
}

// TestLeaseExpiresWhileRenewalBlocked checks the expiry rule with a renewal
// that does not return: at the confirmed expiry the instance stops holding
// the lease and runs OnLose once, another instance takes the lease, and the
// late success of the blocked renewal cannot reactivate the lost ownership.
func TestLeaseExpiresWhileRenewalBlocked(t *testing.T) {
	database := dbtest.New(t)
	clock := newTestClock()
	var acquired, lost atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Lease{DB: database, InstanceID: "a", Now: clock.Now, DeadlineNow: clock.Now, Logger: logger,
		OnAcquire: func(context.Context) { acquired.Add(1) }, OnLose: func() { lost.Add(1) },
		renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	b := &Lease{DB: database, InstanceID: "b", Now: clock.Now, DeadlineNow: clock.Now, Logger: logger}

	var calls atomic.Int32
	blocked := make(chan struct{})
	unblock := make(chan struct{})
	third := make(chan struct{})
	a.renew = func(ctx context.Context) (bool, error) {
		switch calls.Add(1) {
		case 1:
			return a.TryAcquire(ctx)
		case 2:
			// A renewal that does not return and ignores its context.
			close(blocked)
			<-unblock
			return true, nil
		case 3:
			close(third)
			return false, nil
		default:
			return false, nil
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	t.Cleanup(func() {
		release()
		cancel()
		<-done
	})

	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	<-blocked
	if !a.Held() {
		t.Fatal("the instance does not hold the lease before the expiry")
	}
	if held, err := b.TryAcquire(ctx); err != nil || held {
		t.Fatalf("the other instance took a valid lease: %v %v", held, err)
	}

	// The expiry arrives while the renewal is still blocked.
	clock.Advance(TTL)
	if a.Held() {
		t.Fatal("the instance holds the lease at the confirmed expiry")
	}
	eventually(t, "the loss callback", func() bool { return lost.Load() == 1 })
	if held, err := b.TryAcquire(ctx); err != nil || !held {
		t.Fatalf("the other instance could not take the expired lease: %v %v", held, err)
	}

	// The blocked renewal now reports success of the lost period.
	release()
	<-third
	if a.Held() {
		t.Fatal("a late result reactivated the lost ownership")
	}
	if got := lost.Load(); got != 1 {
		t.Fatalf("OnLose ran %d times, want 1", got)
	}
	if got := acquired.Load(); got != 1 {
		t.Fatalf("OnAcquire ran %d times, want 1", got)
	}
}

// TestLeaseAcquireCallbackBoundedByExpiry checks that the work that runs on
// acquisition cannot delay the expiry transition: its context ends at the
// confirmed expiry, so a callback that waits on I/O (a database read) returns
// then, and the loss transition runs OnLose right after it instead of after
// the full I/O timeout.
func TestLeaseAcquireCallbackBoundedByExpiry(t *testing.T) {
	l, lost := newTestLease()
	expireAt := time.Now().Add(200 * time.Millisecond)
	var deadline time.Time
	var hasDeadline bool
	started := make(chan struct{})
	returned := make(chan time.Time, 1)
	l.OnAcquire = func(ctx context.Context) {
		deadline, hasDeadline = ctx.Deadline()
		close(started)
		// Stand-in for a database read that runs until its context ends.
		select {
		case <-ctx.Done():
		case <-time.After(10 * time.Second):
		}
		returned <- time.Now()
	}
	l.init()
	applied := make(chan struct{})
	go func() {
		defer close(applied)
		l.applyRenewal(0, true, "", expireAt)
	}()
	<-started
	// The context is a timeout of the remaining time, so its deadline can
	// differ from the expiry by the time that the call took.
	if !hasDeadline || deadline.Sub(expireAt) > 50*time.Millisecond {
		t.Fatalf("the acquisition context has deadline %v (set: %v), want the expiry %v", deadline, hasDeadline, expireAt)
	}

	// The watcher's expiry check waits for the callback, which ends at the
	// expiry; OnLose runs right after, well before the 10 second I/O bound.
	begin := time.Now()
	time.Sleep(time.Until(expireAt) + 10*time.Millisecond)
	l.expireIfDue()
	<-applied
	if took := time.Since(begin); took > 2*time.Second {
		t.Fatalf("the loss transition took %v after the acquisition started, want about the expiry", took)
	}
	if end := <-returned; end.Sub(expireAt) > time.Second {
		t.Fatalf("the callback returned %v after the expiry", end.Sub(expireAt))
	}
	if l.Held() || lost.Load() != 1 {
		t.Fatalf("held %v, OnLose ran %d times, want not held and once", l.Held(), lost.Load())
	}
}

// TestLeaseRenewalExtendsOwnership checks that a renewal before the deadline
// moves the expiry, so the instance still holds the lease after the first
// deadline passed.
func TestLeaseRenewalExtendsOwnership(t *testing.T) {
	database := dbtest.New(t)
	clock := newTestClock()
	var lost atomic.Int32
	a := &Lease{DB: database, InstanceID: "a", Now: clock.Now, DeadlineNow: clock.Now,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OnLose: func() { lost.Add(1) },
		renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	var calls atomic.Int32
	a.renew = func(ctx context.Context) (bool, error) {
		calls.Add(1)
		return a.TryAcquire(ctx)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}

	// Two more calls start after the clock moves; the first of them has
	// been applied when the second starts.
	clock.Advance(20 * time.Second)
	n := calls.Load()
	eventually(t, "two renewals", func() bool { return calls.Load() >= n+2 })
	// Past the first deadline (30 s after the acquisition).
	clock.Advance(15 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if !a.Held() || lost.Load() != 0 {
		t.Fatalf("held %v, OnLose ran %d times after the renewal", a.Held(), lost.Load())
	}
}

// TestLeaseReleaseDrain checks that releasing the lease still gives it up at
// once: OnLose runs once, the row is deleted and another instance takes it
// without waiting for the TTL.
func TestLeaseReleaseDrain(t *testing.T) {
	database := dbtest.New(t)
	clock := newTestClock()
	var lost atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Lease{DB: database, InstanceID: "a", Now: clock.Now, DeadlineNow: clock.Now, Logger: logger,
		OnLose: func() { lost.Add(1) }, renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	b := &Lease{DB: database, InstanceID: "b", Now: clock.Now, DeadlineNow: clock.Now, Logger: logger}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); a.Run(ctx) }()
	if err := a.Wait(ctx); err != nil {
		t.Fatal(err)
	}
	if held, err := b.TryAcquire(context.Background()); err != nil || held {
		t.Fatalf("the other instance took a valid lease: %v %v", held, err)
	}
	// The drain ends the context of Run, which releases the lease.
	cancel()
	<-done
	if a.Held() || lost.Load() != 1 {
		t.Fatalf("held %v, OnLose ran %d times after the drain", a.Held(), lost.Load())
	}
	if held, err := b.TryAcquire(context.Background()); err != nil || !held {
		t.Fatalf("the other instance could not take the released lease: %v %v", held, err)
	}
}
