// SPDX-License-Identifier: FSL-1.1-ALv2

// Package lease keeps the single-active-instance lease. An instance accepts
// agent channels and runs heartbeat timers and state transitions only while
// it holds the lease. The holder renews it every RenewInterval; the lease
// expires TTL after its last renewal, so a crashed holder loses it.
package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Lease timing from the control plane contract.
const (
	Name          = "ownsand-api"
	TTL           = 30 * time.Second
	RenewInterval = 10 * time.Second
)

// Backend stores the lease.
type Backend interface {
	AcquireLease(ctx context.Context, name, holder string, ttl time.Duration) (bool, error)
	ReleaseLease(ctx context.Context, name, holder string) error
}

// Manager acquires and renews the lease.
type Manager struct {
	Backend Backend
	Holder  string
	Logger  *slog.Logger
	// OnAcquired runs when this instance becomes the holder; OnLost runs when
	// it stops being the holder (renewal failure, expiry or Release).
	OnAcquired func()
	OnLost     func()
	Interval   time.Duration

	mu       sync.Mutex
	held     bool
	released bool
	lastOK   time.Time
}

// Held reports whether this instance holds the lease now.
func (m *Manager) Held() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.held && time.Since(m.lastOK) < TTL
}

// Run acquires and renews the lease until ctx ends or Release is called.
func (m *Manager) Run(ctx context.Context) {
	interval := m.Interval
	if interval == 0 {
		interval = RenewInterval
	}
	m.tick(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *Manager) tick(ctx context.Context) {
	m.mu.Lock()
	if m.released {
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	attemptStart := time.Now()
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	ok, err := m.Backend.AcquireLease(callCtx, Name, m.Holder, TTL)
	cancel()
	if err != nil {
		m.Logger.Warn("lease_renew_failed", slog.Any("error", err))
	}
	m.mu.Lock()
	was := m.held
	if m.released {
		m.mu.Unlock()
		return
	}
	switch {
	case err == nil && ok:
		m.held = true
		m.lastOK = attemptStart
	case err == nil && !ok:
		m.held = false
	default:
		// A failed renewal keeps the lease until it would expire.
		if time.Since(m.lastOK) >= TTL-RenewInterval {
			m.held = false
		}
	}
	now := m.held
	m.mu.Unlock()
	if !was && now {
		m.Logger.Info("lease_acquired", slog.String("holder", m.Holder))
		if m.OnAcquired != nil {
			m.OnAcquired()
		}
	}
	if was && !now {
		m.Logger.Warn("lease_lost", slog.String("holder", m.Holder))
		if m.OnLost != nil {
			m.OnLost()
		}
	}
}

// Release gives up the lease (at drain) and stops renewing it.
func (m *Manager) Release(ctx context.Context) {
	m.mu.Lock()
	was := m.held
	m.held = false
	m.released = true
	m.mu.Unlock()
	if err := m.Backend.ReleaseLease(ctx, Name, m.Holder); err != nil {
		m.Logger.Warn("lease_release_failed", slog.Any("error", err))
	}
	if was {
		m.Logger.Info("lease_released", slog.String("holder", m.Holder))
		if m.OnLost != nil {
			m.OnLost()
		}
	}
}
