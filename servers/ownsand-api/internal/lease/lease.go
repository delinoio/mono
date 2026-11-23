// SPDX-License-Identifier: FSL-1.1-ALv2

// Package lease holds the instance lease in Postgres. An instance accepts
// agent channels and runs heartbeat timers and state transitions only while
// it holds the lease. The holder renews it every 10 seconds; it expires 30
// seconds after the last renewal, so a crashed holder loses it. A holder
// that cannot confirm a renewal stops owning the lease at the expire_time of
// its last successful renewal, whether or not the renewal call has returned.
// A draining instance releases it at once. The owning contract is the
// Runtime and Language section of docs/servers-ownsand-api-foundation.md.
package lease

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/delinoio/mono/servers/ownsand-api/internal/db"
)

// Lease timing of the contract.
const (
	Name          = "control-plane"
	TTL           = 30 * time.Second
	RenewInterval = 10 * time.Second
	// ExpiryPollInterval bounds how long the expiry watcher sleeps; it also
	// wakes at the deadline when that is sooner.
	ExpiryPollInterval = time.Second
)

// Reasons of the lease_lost log.
const (
	reasonExpired   = "expired"
	reasonNotHolder = "not_holder"
	reasonReleased  = "released"
)

// Lease is one instance's view of the lease.
type Lease struct {
	DB         *db.DB
	InstanceID string
	Logger     *slog.Logger
	// DeadlineNow is the clock of the local ownership deadline; it defaults
	// to time.Now. It never reaches the database: the expire_time that an
	// instance writes and the expiry that it compares against are both
	// computed with the database clock, so every instance decides takeover
	// on one shared time reference.
	DeadlineNow func() time.Time
	// OnAcquire runs when the instance gains the lease; OnLose runs when it
	// loses or releases it. The context of OnAcquire ends at the confirmed
	// expiry of the new ownership period: OnAcquire runs while the
	// transition lock is held, and the expiry transition (and so OnLose)
	// waits for it, so any I/O of OnAcquire must stop with its context.
	OnAcquire func(ctx context.Context)
	OnLose    func()

	// Test seams of this package: the renewal call, the renewal interval and
	// the watcher poll interval. Zero values select the production behavior.
	renew      func(ctx context.Context) (bool, error)
	renewEvery time.Duration
	poll       time.Duration

	// transitionMu serializes ownership transitions together with their
	// callbacks, so OnAcquire and OnLose of different transitions never
	// overlap or run out of order. It is taken before mu and never while
	// holding mu.
	transitionMu sync.Mutex

	mu       sync.Mutex
	held     bool
	expireAt time.Time // confirmed expiry of the current ownership period
	epoch    uint64    // changes at every acquisition and every loss
	acquired chan struct{}
}

func (l *Lease) init() {
	l.mu.Lock()
	if l.acquired == nil {
		l.acquired = make(chan struct{})
	}
	l.mu.Unlock()
}

func (l *Lease) deadlineNow() time.Time {
	if l.DeadlineNow != nil {
		return l.DeadlineNow()
	}
	return time.Now()
}

// heldLocked reports ownership: the flag is set and the confirmed expiry has
// not passed. The expiry rule applies here as well as in the watcher, so an
// instance never reports the lease after its deadline, even in the interval
// before the watcher runs the loss transition. l.mu must be held.
func (l *Lease) heldLocked() bool {
	return l.held && l.deadlineNow().Before(l.expireAt)
}

// Held reports whether this instance holds the lease.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heldLocked()
}

// WhileHeld runs fn while this instance holds the lease and reports whether
// it ran. It holds the ownership lock for the whole call, and the transition
// that clears ownership takes the same lock before it runs OnLose. So fn
// either completes before the transition (and OnLose sees its effects) or
// is not run at all. Agent channel admission uses it to register a channel
// in the hub as one step with the ownership check.
//
// fn must be short, in-memory work: it must not do database or stream I/O,
// block, or call a method of the lease (Held, WhileHeld, Wait), because the
// lock is not reentrant and the lock stalls the renewal loop.
func (l *Lease) WhileHeld(fn func()) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.heldLocked() {
		return false
	}
	fn()
	return true
}

// Wait blocks until the instance holds the lease or ctx ends. It returns nil
// only while the ownership period is current: the acquisition channel stays
// closed after the confirmed expiry until the watcher runs the loss
// transition, so Wait checks the deadline itself and, in that interval, runs
// the transition (which installs a fresh channel) instead of returning for an
// expired acquisition or spinning on the closed channel.
func (l *Lease) Wait(ctx context.Context) error {
	l.init()
	for {
		l.mu.Lock()
		if l.heldLocked() {
			l.mu.Unlock()
			return nil
		}
		if l.held {
			// Expired, but the loss is not recorded yet. expireIfDue is
			// idempotent and serialized with the other transitions.
			l.mu.Unlock()
			l.expireIfDue()
			if err := ctx.Err(); err != nil {
				return err
			}
			continue
		}
		ch := l.acquired
		l.mu.Unlock()
		select {
		case <-ch:
			// An acquisition closed the channel; loop to confirm that the
			// period is still current.
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// TryAcquire takes or renews the lease once and reports whether this
// instance holds it. It does not change the local ownership state; Run does.
//
// The statement uses the database clock (now() is the time of the statement's
// transaction) for the written expire_time and for the expiry check, never an
// application clock. A wall-clock step of one instance therefore cannot let it
// take a lease that another instance renewed, or delay its takeover of an
// abandoned one, and the local ownership deadline (a monotonic reading before
// the call) stays no later than the expire_time that the database wrote.
func (l *Lease) TryAcquire(ctx context.Context) (bool, error) {
	var holder string
	err := l.DB.Pool.QueryRow(ctx, `INSERT INTO instance_leases (lease_name, holder, expire_time) VALUES ($1, $2, now() + make_interval(secs => $3))
		ON CONFLICT (lease_name) DO UPDATE SET holder = EXCLUDED.holder, expire_time = EXCLUDED.expire_time
		WHERE instance_leases.holder = EXCLUDED.holder OR instance_leases.expire_time <= now()
		RETURNING holder`, Name, l.InstanceID, TTL.Seconds()).Scan(&holder)
	if err != nil {
		// No row: another instance holds a valid lease.
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return holder == l.InstanceID, nil
}

// loseLocked ends the current ownership period. The caller holds
// transitionMu and mu and has checked that the instance owns the lease;
// loseLocked releases mu and runs the callback.
func (l *Lease) loseLocked(reason string) {
	l.held = false
	l.expireAt = time.Time{}
	l.epoch++
	l.acquired = make(chan struct{})
	l.mu.Unlock()
	l.Logger.Warn("lease_lost", slog.String("instance_id", l.InstanceID), slog.String("reason", reason))
	if l.OnLose != nil {
		l.OnLose()
	}
}

// lose ends the ownership period when there is one and reports whether it did.
func (l *Lease) lose(reason string) bool {
	l.init()
	l.transitionMu.Lock()
	defer l.transitionMu.Unlock()
	l.mu.Lock()
	if !l.held {
		l.mu.Unlock()
		return false
	}
	l.loseLocked(reason)
	return true
}

// expireIfDue ends the ownership period when the confirmed expiry has passed.
func (l *Lease) expireIfDue() {
	l.init()
	l.transitionMu.Lock()
	defer l.transitionMu.Unlock()
	l.mu.Lock()
	if !l.held || l.deadlineNow().Before(l.expireAt) {
		l.mu.Unlock()
		return
	}
	l.loseLocked(reasonExpired)
}

// applyRenewal applies the result of a renewal call that started in the
// ownership period epoch and confirmed the lease until expireAt when ok. A
// result from an earlier period (the instance lost or released the lease
// while the call was in flight) is dropped, so a late success cannot
// reactivate it, and a result that arrives after the expiry does not extend
// it. A fresh acquisition starts a new period. The OnAcquire callback runs
// under ctx, the run context of the lease workers, so that stopping them
// cancels an acquisition callback that is still in progress.
func (l *Lease) applyRenewal(ctx context.Context, epoch uint64, ok bool, reason string, expireAt time.Time) {
	l.init()
	l.transitionMu.Lock()
	defer l.transitionMu.Unlock()
	l.mu.Lock()
	if l.epoch != epoch {
		l.mu.Unlock()
		return
	}
	now := l.deadlineNow()
	switch {
	case l.held && ok && now.Before(l.expireAt):
		if expireAt.After(l.expireAt) {
			l.expireAt = expireAt
		}
		l.mu.Unlock()
	case l.held && ok:
		l.loseLocked(reasonExpired)
	case l.held:
		l.loseLocked(reason)
	case ok && now.Before(expireAt):
		l.held = true
		l.expireAt = expireAt
		l.epoch++
		close(l.acquired)
		// OnAcquire holds transitionMu, which the expiry transition needs
		// before it can run OnLose. Bound it by the confirmed expiry, so
		// that its I/O cannot keep the channels admitted in the remaining
		// ownership window open past it. It also ends when the lease
		// workers stop, because Run releases the lease (which needs
		// transitionMu) only after the renewal that runs it returns.
		acquireCtx, cancel := context.WithTimeout(ctx, expireAt.Sub(now))
		defer cancel()
		l.mu.Unlock()
		l.Logger.Info("lease_acquired", slog.String("instance_id", l.InstanceID))
		if l.OnAcquire != nil {
			l.OnAcquire(acquireCtx)
		}
	default:
		l.mu.Unlock()
	}
}

// snapshot returns the ownership period and its expiry for the next renewal.
func (l *Lease) snapshot() (epoch uint64, expireAt time.Time, held bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.epoch, l.expireAt, l.held
}

// watch ends the ownership period at the confirmed expiry, even when the
// renewal call has not returned. It wakes at the deadline or after the poll
// interval, whichever is sooner, so it does not depend on the renewal loop.
func (l *Lease) watch(ctx context.Context) {
	poll := l.poll
	if poll <= 0 {
		poll = ExpiryPollInterval
	}
	timer := time.NewTimer(poll)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		l.expireIfDue()
		wait := poll
		if _, expireAt, held := l.snapshot(); held {
			if remaining := expireAt.Sub(l.deadlineNow()); remaining < wait {
				wait = max(remaining, time.Millisecond)
			}
		}
		timer.Reset(wait)
	}
}

// renewOnce takes or renews the lease once. A renewal of a held lease is
// bounded by the confirmed expiry, because a result after it is useless. A
// failed renewal keeps the lease until that expiry.
func (l *Lease) renewOnce(ctx context.Context) {
	epoch, expireAt, held := l.snapshot()
	if held && !l.deadlineNow().Before(expireAt) {
		l.expireIfDue()
		epoch, expireAt, held = l.snapshot()
	}
	// The expiry of a successful call is TTL after the moment before the
	// call, which is no later than the expire_time that TryAcquire wrote.
	start := l.deadlineNow()
	timeout := TTL
	if held {
		timeout = expireAt.Sub(start)
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	renew := l.renew
	if renew == nil {
		renew = l.TryAcquire
	}
	ok, err := renew(callCtx)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		// An error does not confirm that another holder owns the lease, so
		// it changes no state: a held lease stays held until the confirmed
		// expiry, which the watcher enforces, and a later successful
		// renewal moves that expiry. Only a definitive refusal ends
		// ownership at once.
		l.Logger.Warn("lease_renew_failed", slog.Any("error", err))
		return
	}
	l.applyRenewal(ctx, epoch, ok, reasonNotHolder, start.Add(TTL))
}

// Run keeps the lease until ctx ends and then releases it.
func (l *Lease) Run(ctx context.Context) {
	l.init()
	interval := l.renewEvery
	if interval <= 0 {
		interval = RenewInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var watcher sync.WaitGroup
	watcher.Add(1)
	go func() { defer watcher.Done(); l.watch(ctx) }()
	defer watcher.Wait()
	for {
		l.renewOnce(ctx)
		select {
		case <-ctx.Done():
			l.Release()
			return
		case <-ticker.C:
		}
	}
}

// Release gives the lease up at once, for example when the instance starts
// to drain. It ends a local ownership period when there is one (and runs
// OnLose then), and it always deletes the row of this instance. The row can
// exist without a confirmed ownership period: an acquisition whose call was
// canceled or failed after the database committed it never set the local
// ownership, and its row would otherwise block the replacement until the
// TTL ends. The delete is scoped to this holder, so the row of another
// instance stays.
func (l *Lease) Release() {
	l.lose(reasonReleased)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.DB.Pool.Exec(ctx, "DELETE FROM instance_leases WHERE lease_name = $1 AND holder = $2", Name, l.InstanceID); err != nil {
		l.Logger.Warn("lease_release_failed", slog.Any("error", err))
	}
}
