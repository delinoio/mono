// SPDX-License-Identifier: FSL-1.1-ALv2

package lease

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
	"github.com/delinoio/mono/servers/ownsand-api/internal/db/dbtest"
)

// testClock is a settable clock for the local ownership deadline. The
// database timestamps use the database clock and never this one.
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
	l.applyRenewal(context.Background(), epoch, true, "", expireAt)
	if !l.Held() {
		t.Fatal("the acquisition did not give the lease")
	}
}

// setRowExpiry moves the expire_time of the lease row to the database time
// plus offset, as the passing of database time would.
func setRowExpiry(t *testing.T, database *db.DB, offset time.Duration) {
	t.Helper()
	tag, err := database.Pool.Exec(context.Background(),
		"UPDATE instance_leases SET expire_time = now() + make_interval(secs => $2) WHERE lease_name = $1", Name, offset.Seconds())
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("could not set the row expiry: rows %d, error %v", tag.RowsAffected(), err)
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
	l.applyRenewal(context.Background(), epoch, true, "", expireAt.Add(TTL))
	if l.Held() {
		t.Fatal("a result after the expiry extended the ownership")
	}
	if got := lost.Load(); got != 1 {
		t.Fatalf("OnLose ran %d times, want 1", got)
	}
	// The result of the ended period cannot restore it, and a second
	// expiry check does not run OnLose again.
	l.applyRenewal(context.Background(), epoch, true, "", clock.Now().Add(TTL))
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
	a := &Lease{DB: database, InstanceID: "a", DeadlineNow: clock.Now, Logger: logger,
		OnAcquire: func(context.Context) { acquired.Add(1) }, OnLose: func() { lost.Add(1) },
		renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	b := &Lease{DB: database, InstanceID: "b", DeadlineNow: clock.Now, Logger: logger}

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

	// The expiry arrives while the renewal is still blocked: the local
	// deadline passes and the database time passes the row's expire_time.
	clock.Advance(TTL)
	setRowExpiry(t, database, -time.Second)
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

// TestWaitIgnoresExpiredAcquisition checks that Wait does not report a lease
// whose confirmed expiry has passed before the watcher recorded the loss: the
// acquisition channel is still closed then. Wait must record the loss itself,
// block until a current acquisition or the end of its context, and not spin.
func TestWaitIgnoresExpiredAcquisition(t *testing.T) {
	clock := newTestClock()
	l, lost := newTestLease()
	l.DeadlineNow = clock.Now
	acquireFor(t, l, clock.Now().Add(TTL))

	// A current acquisition returns at once.
	if err := l.Wait(context.Background()); err != nil {
		t.Fatalf("Wait with a current lease: %v", err)
	}

	// The expiry passes and no watcher runs, so the loss is not recorded yet.
	clock.Advance(TTL)
	if l.Held() {
		t.Fatal("the instance holds the lease at the confirmed expiry")
	}

	// Wait ends only with its own context. Count the loops through the loss
	// callback: a spin on the closed channel would never record the loss, and
	// a Wait that returned nil would fail the check below.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := l.Wait(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Wait for an expired lease returned %v, want the context error", err)
	}
	if got := lost.Load(); got != 1 {
		t.Fatalf("OnLose ran %d times, want 1 (Wait records the expiry once)", got)
	}

	// A waiter that is blocked returns when a new period is acquired.
	waitDone := make(chan error, 1)
	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	go func() { waitDone <- l.Wait(waitCtx) }()
	select {
	case err := <-waitDone:
		t.Fatalf("Wait returned %v without a current lease", err)
	case <-time.After(50 * time.Millisecond):
	}
	acquireFor(t, l, clock.Now().Add(TTL))
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("Wait after the new acquisition: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after the new acquisition")
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
		l.applyRenewal(context.Background(), 0, true, "", expireAt)
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
	a := &Lease{DB: database, InstanceID: "a", DeadlineNow: clock.Now,
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

// TestLeaseRunCancelStopsAcquireCallback checks that stopping the lease
// workers cancels an acquisition callback that is still running, so the
// release and OnLose do not wait for the callback's own I/O bound (the
// cursor read timeout) or for the lease expiry.
func TestLeaseRunCancelStopsAcquireCallback(t *testing.T) {
	database := dbtest.New(t)
	var lost atomic.Int32
	l := &Lease{DB: database, InstanceID: "a",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), OnLose: func() { lost.Add(1) },
		renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	started := make(chan struct{})
	l.OnAcquire = func(ctx context.Context) {
		close(started)
		// Stand-in for a cursor read that runs until its context ends.
		select {
		case <-ctx.Done():
		case <-time.After(20 * time.Second):
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); l.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the acquisition callback did not start")
	}
	begin := time.Now()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel while the acquisition callback was running")
	}
	if took := time.Since(begin); took > 5*time.Second {
		t.Fatalf("Run took %v to return after cancel", took)
	}
	if l.Held() || lost.Load() != 1 {
		t.Fatalf("held %v, OnLose ran %d times, want not held and once", l.Held(), lost.Load())
	}
	var rows int
	if err := database.Pool.QueryRow(context.Background(), "SELECT count(*) FROM instance_leases WHERE lease_name = $1 AND holder = $2", Name, "a").Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 0 {
		t.Fatalf("the release left %d lease rows", rows)
	}
}

// TestTakeoverUsesDatabaseTime checks that the takeover decision and the
// written expire_time use the database clock only: an application clock that
// is an hour ahead cannot take a lease that is valid in database time, and one
// that is an hour behind cannot delay the takeover of an expired lease.
func TestTakeoverUsesDatabaseTime(t *testing.T) {
	database := dbtest.New(t)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx := context.Background()
	aheadClock, behindClock := newTestClock(), newTestClock()
	aheadClock.Advance(time.Hour)
	behindClock.Advance(-time.Hour)
	a := &Lease{DB: database, InstanceID: "a", Logger: logger, DeadlineNow: behindClock.Now}
	ahead := &Lease{DB: database, InstanceID: "ahead", Logger: logger, DeadlineNow: aheadClock.Now}
	behind := &Lease{DB: database, InstanceID: "behind", Logger: logger, DeadlineNow: behindClock.Now}

	if held, err := a.TryAcquire(ctx); err != nil || !held {
		t.Fatalf("a could not acquire: %v %v", held, err)
	}
	// The written expire_time is the database time plus the TTL, whatever the
	// application clock says.
	var seconds float64
	if err := database.Pool.QueryRow(ctx, "SELECT extract(epoch FROM expire_time - now()) FROM instance_leases WHERE lease_name = $1", Name).Scan(&seconds); err != nil {
		t.Fatal(err)
	}
	remaining := time.Duration(seconds * float64(time.Second))
	if remaining > TTL || remaining < TTL-5*time.Second {
		t.Fatalf("expire_time is %v after the database time, want about %v", remaining, TTL)
	}

	// A forward wall-clock step of a contender does not expire the row.
	if held, err := ahead.TryAcquire(ctx); err != nil || held {
		t.Fatalf("a contender with a clock one hour ahead took a valid lease: %v %v", held, err)
	}
	if held, err := a.TryAcquire(ctx); err != nil || !held {
		t.Fatalf("the holder could not renew: %v %v", held, err)
	}

	// The row expires in database time; a contender whose clock is an hour
	// behind takes it at once.
	setRowExpiry(t, database, -time.Second)
	if held, err := behind.TryAcquire(ctx); err != nil || !held {
		t.Fatalf("a contender with a clock one hour behind could not take an expired lease: %v %v", held, err)
	}
	// The previous holder cannot take it back while it is valid.
	if held, err := a.TryAcquire(ctx); err != nil || held {
		t.Fatalf("the previous holder took a valid lease: %v %v", held, err)
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
	a := &Lease{DB: database, InstanceID: "a", DeadlineNow: clock.Now, Logger: logger,
		OnLose: func() { lost.Add(1) }, renewEvery: 20 * time.Millisecond, poll: 5 * time.Millisecond}
	b := &Lease{DB: database, InstanceID: "b", DeadlineNow: clock.Now, Logger: logger}
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

// TestLeaseReleaseAfterUncertainAcquisition checks that a drain deletes the
// row of an acquisition that the database committed but the instance never
// confirmed (the call failed or was canceled), without OnLose, and that
// another holder takes the lease at once.
func TestLeaseReleaseAfterUncertainAcquisition(t *testing.T) {
	database := dbtest.New(t)
	clock := newTestClock()
	var lost atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Lease{DB: database, InstanceID: "a", DeadlineNow: clock.Now, Logger: logger,
		OnLose: func() { lost.Add(1) }}
	b := &Lease{DB: database, InstanceID: "b", DeadlineNow: clock.Now, Logger: logger}
	// The renewal commits the row and then reports an error, as a call does
	// when the connection breaks after the commit.
	a.renew = func(ctx context.Context) (bool, error) {
		if _, err := a.TryAcquire(context.Background()); err != nil {
			return false, err
		}
		return false, errors.New("connection reset after commit")
	}
	a.renewOnce(context.Background())
	if a.Held() {
		t.Fatal("an unconfirmed acquisition set the ownership")
	}
	if held, err := b.TryAcquire(context.Background()); err != nil || held {
		t.Fatalf("the other instance took a valid lease: %v %v", held, err)
	}
	a.Release()
	if lost.Load() != 0 {
		t.Fatalf("OnLose ran %d times for an unconfirmed acquisition", lost.Load())
	}
	if held, err := b.TryAcquire(context.Background()); err != nil || !held {
		t.Fatalf("the other instance could not take the released lease: %v %v", held, err)
	}
}

// TestLeaseReleaseKeepsOtherHolderRow checks that a drain of an instance that
// does not own the lease leaves the row of the current holder.
func TestLeaseReleaseKeepsOtherHolderRow(t *testing.T) {
	database := dbtest.New(t)
	clock := newTestClock()
	var lost atomic.Int32
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	a := &Lease{DB: database, InstanceID: "a", DeadlineNow: clock.Now, Logger: logger,
		OnLose: func() { lost.Add(1) }}
	b := &Lease{DB: database, InstanceID: "b", DeadlineNow: clock.Now, Logger: logger}
	if held, err := b.TryAcquire(context.Background()); err != nil || !held {
		t.Fatalf("b could not acquire: %v %v", held, err)
	}
	a.Release()
	if lost.Load() != 0 {
		t.Fatalf("OnLose ran %d times for an instance that never held the lease", lost.Load())
	}
	if held, err := b.TryAcquire(context.Background()); err != nil || !held {
		t.Fatalf("the row of the other holder was deleted: %v %v", held, err)
	}
	var holder string
	if err := database.Pool.QueryRow(context.Background(), "SELECT holder FROM instance_leases WHERE lease_name = $1", Name).Scan(&holder); err != nil || holder != "b" {
		t.Fatalf("row holder %q, error %v", holder, err)
	}
}

// newRenewTestLease returns a lease with a test clock, a text log buffer and
// a renewal stub that returns the result of next.
func newRenewTestLease(next func() (bool, error)) (*Lease, *testClock, *atomic.Int32, *bytes.Buffer) {
	clock := newTestClock()
	l, lost := newTestLease()
	var logs bytes.Buffer
	l.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	l.DeadlineNow = clock.Now
	l.renew = func(context.Context) (bool, error) { return next() }
	l.init()
	return l, clock, lost, &logs
}

// TestRenewalErrorKeepsLeaseUntilConfirmedExpiry checks that a failed
// renewal does not end ownership, that ownership still ends at the
// unchanged confirmed expiry, and that a later success moves the expiry.
func TestRenewalErrorKeepsLeaseUntilConfirmedExpiry(t *testing.T) {
	fail := true
	l, clock, lost, logs := newRenewTestLease(func() (bool, error) {
		if fail {
			return false, errors.New("connection reset")
		}
		return true, nil
	})
	start := clock.Now()
	acquireFor(t, l, start.Add(TTL))
	_, confirmed, _ := l.snapshot()

	clock.Advance(RenewInterval)
	l.renewOnce(context.Background())
	if !l.Held() || lost.Load() != 0 {
		t.Fatalf("held %v, OnLose ran %d times after a renewal error", l.Held(), lost.Load())
	}
	if _, got, _ := l.snapshot(); !got.Equal(confirmed) {
		t.Fatalf("expiry moved to %v, want %v", got, confirmed)
	}
	if !strings.Contains(logs.String(), "lease_renew_failed") || strings.Contains(logs.String(), "lease_lost") {
		t.Fatalf("unexpected logs: %s", logs.String())
	}

	// A success after the error and before the expiry moves the expiry.
	fail = false
	clock.Advance(RenewInterval)
	renewedAt := clock.Now()
	l.renewOnce(context.Background())
	if _, got, _ := l.snapshot(); !got.Equal(renewedAt.Add(TTL)) {
		t.Fatalf("expiry %v, want %v", got, renewedAt.Add(TTL))
	}
	clock.Advance(TTL - time.Second)
	if !l.Held() || lost.Load() != 0 {
		t.Fatalf("ownership did not survive the original deadline: held %v, OnLose %d", l.Held(), lost.Load())
	}

	// Without a further success, ownership ends once, with reason expired.
	fail = true
	l.renewOnce(context.Background())
	clock.Advance(time.Second)
	l.expireIfDue()
	l.expireIfDue()
	if l.Held() || lost.Load() != 1 {
		t.Fatalf("held %v, OnLose ran %d times, want released once", l.Held(), lost.Load())
	}
	if !strings.Contains(logs.String(), "reason=expired") {
		t.Fatalf("missing lease_lost reason=expired: %s", logs.String())
	}
}

// TestRenewalRefusalLosesLeaseAtOnce checks that a definitive (false, nil)
// refusal still ends ownership immediately and is not a renewal failure.
func TestRenewalRefusalLosesLeaseAtOnce(t *testing.T) {
	l, clock, lost, logs := newRenewTestLease(func() (bool, error) { return false, nil })
	acquireFor(t, l, clock.Now().Add(TTL))
	clock.Advance(RenewInterval)
	l.renewOnce(context.Background())
	if l.Held() || lost.Load() != 1 {
		t.Fatalf("held %v, OnLose ran %d times, want lost once", l.Held(), lost.Load())
	}
	if !strings.Contains(logs.String(), "reason=not_holder") || strings.Contains(logs.String(), "lease_renew_failed") {
		t.Fatalf("unexpected logs: %s", logs.String())
	}
}

// TestRenewalErrorWhileNotHeldChangesNothing checks that a failed
// acquisition attempt leaves the lease unheld and runs no callback.
func TestRenewalErrorWhileNotHeldChangesNothing(t *testing.T) {
	l, _, lost, logs := newRenewTestLease(func() (bool, error) { return false, errors.New("down") })
	l.renewOnce(context.Background())
	if l.Held() || lost.Load() != 0 {
		t.Fatalf("held %v, OnLose ran %d times", l.Held(), lost.Load())
	}
	if strings.Contains(logs.String(), "lease_lost") {
		t.Fatalf("unexpected lease_lost log: %s", logs.String())
	}
}
