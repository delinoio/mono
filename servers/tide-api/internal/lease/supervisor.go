// SPDX-License-Identifier: FSL-1.1-ALv2

package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/delinoio/mono/internal/backoff"
)

// Component is a background component that runs only on the lease holder,
// for example the dispatcher, a vendor watcher, a Claude Code orchestrator,
// a reconciler or the daily jobs. Later layers register components with
// app.Registry.AddLeaseComponent.
//
// Run starts when this instance acquires the lease and must return soon
// after ctx ends, which happens when the instance loses or releases the
// lease (also when it starts to drain). The lease transition waits for every
// component to return, so the old instance of a deployment stops its
// components before a new holder can start them. A Run that returns while
// ctx is still active (with or without an error) is restarted with
// exponential backoff from 1 second to 1 minute. Run must not assume that it
// is the only run in the life of the process: the instance can lose and
// acquire the lease again, and Run then starts again.
type Component interface {
	// Name is the stable component name in log records.
	Name() string
	Run(ctx context.Context) error
}

// ComponentFunc adapts a function to Component.
type ComponentFunc struct {
	ComponentName string
	RunFunc       func(ctx context.Context) error
}

// Name returns the component name.
func (c ComponentFunc) Name() string { return c.ComponentName }

// Run runs the function.
func (c ComponentFunc) Run(ctx context.Context) error { return c.RunFunc(ctx) }

// Restart backoff of a component that returned while the lease is held.
const (
	restartMin = time.Second
	restartMax = time.Minute
)

// Supervisor runs the registered components while the lease is held. Set
// Lease.OnAcquire to Start and Lease.OnLose to Stop.
type Supervisor struct {
	Logger *slog.Logger

	mu         sync.Mutex
	components []Component
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	// restartMin overrides the first restart delay (tests).
	restartMin time.Duration
}

// Add registers a component. Components added while the lease is held start
// at the next acquisition.
func (s *Supervisor) Add(c Component) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.components = append(s.components, c)
}

// Start starts every component with a context that Stop cancels. A second
// Start without Stop does nothing.
func (s *Supervisor) Start() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	for _, c := range s.components {
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.supervise(ctx, c)
		}()
	}
}

// Stop cancels the components and waits until every Run has returned.
func (s *Supervisor) Stop() {
	s.mu.Lock()
	cancel := s.cancel
	s.cancel = nil
	s.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	s.wg.Wait()
}

func (s *Supervisor) supervise(ctx context.Context, c Component) {
	min := restartMin
	if s.restartMin > 0 {
		min = s.restartMin
	}
	b := backoff.Backoff{Min: min, Max: restartMax}
	for {
		s.Logger.Info("lease_component_started", slog.String("component", c.Name()))
		err := c.Run(ctx)
		if ctx.Err() != nil {
			s.Logger.Info("lease_component_stopped", slog.String("component", c.Name()))
			return
		}
		delay := b.Next()
		s.Logger.Warn("lease_component_restarting", slog.String("component", c.Name()),
			slog.Any("error", err), slog.Int64("delay_ms", delay.Milliseconds()))
		if backoff.Sleep(ctx, delay) != nil {
			s.Logger.Info("lease_component_stopped", slog.String("component", c.Name()))
			return
		}
	}
}
