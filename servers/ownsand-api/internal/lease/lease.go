// SPDX-License-Identifier: FSL-1.1-ALv2

// Package lease holds the instance lease in Postgres. An instance accepts
// agent channels and runs heartbeat timers and state transitions only while
// it holds the lease. The holder renews it every 10 seconds; it expires 30
// seconds after the last renewal, so a crashed holder loses it. A draining
// instance releases it at once. The owning contract is the Runtime and
// Language section of docs/servers-ownsand-api-foundation.md.
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
)

// Lease is one instance's view of the lease.
type Lease struct {
	DB         *db.DB
	InstanceID string
	Now        func() time.Time
	Logger     *slog.Logger
	// OnAcquire runs when the instance gains the lease; OnLose runs when it
	// loses or releases it.
	OnAcquire func()
	OnLose    func()

	mu       sync.Mutex
	held     bool
	acquired chan struct{}
}

func (l *Lease) init() {
	l.mu.Lock()
	if l.acquired == nil {
		l.acquired = make(chan struct{})
	}
	l.mu.Unlock()
}

// Held reports whether this instance holds the lease.
func (l *Lease) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.held
}

// Wait blocks until the instance holds the lease or ctx ends.
func (l *Lease) Wait(ctx context.Context) error {
	l.init()
	l.mu.Lock()
	ch := l.acquired
	l.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TryAcquire takes or renews the lease once and reports whether this
// instance holds it.
func (l *Lease) TryAcquire(ctx context.Context) (bool, error) {
	now := l.Now()
	var holder string
	err := l.DB.Pool.QueryRow(ctx, `INSERT INTO instance_leases (lease_name, holder, expire_time) VALUES ($1, $2, $3)
		ON CONFLICT (lease_name) DO UPDATE SET holder = EXCLUDED.holder, expire_time = EXCLUDED.expire_time
		WHERE instance_leases.holder = EXCLUDED.holder OR instance_leases.expire_time <= $4
		RETURNING holder`, Name, l.InstanceID, now.Add(TTL), now).Scan(&holder)
	if err != nil {
		// No row: another instance holds a valid lease.
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return holder == l.InstanceID, nil
}

func (l *Lease) set(held bool) {
	l.init()
	l.mu.Lock()
	changed := l.held != held
	l.held = held
	if changed && held {
		close(l.acquired)
	}
	if changed && !held {
		l.acquired = make(chan struct{})
	}
	l.mu.Unlock()
	if !changed {
		return
	}
	if held {
		l.Logger.Info("lease_acquired", slog.String("instance_id", l.InstanceID))
		if l.OnAcquire != nil {
			l.OnAcquire()
		}
		return
	}
	l.Logger.Warn("lease_lost", slog.String("instance_id", l.InstanceID))
	if l.OnLose != nil {
		l.OnLose()
	}
}

// Run keeps the lease until ctx ends and then releases it.
func (l *Lease) Run(ctx context.Context) {
	l.init()
	ticker := time.NewTicker(RenewInterval)
	defer ticker.Stop()
	for {
		held, err := l.TryAcquire(ctx)
		if err != nil && ctx.Err() == nil {
			l.Logger.Warn("lease_renew_failed", slog.Any("error", err))
			// Keep the lease until it would have expired; a failed renewal
			// after TTL means another instance may take it.
			held = false
		}
		if ctx.Err() == nil {
			l.set(held)
		}
		select {
		case <-ctx.Done():
			l.Release()
			return
		case <-ticker.C:
		}
	}
}

// Release gives the lease up at once, for example when the instance starts
// to drain.
func (l *Lease) Release() {
	if !l.Held() {
		return
	}
	l.set(false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := l.DB.Pool.Exec(ctx, "DELETE FROM instance_leases WHERE lease_name = $1 AND holder = $2", Name, l.InstanceID); err != nil {
		l.Logger.Warn("lease_release_failed", slog.Any("error", err))
	}
}
