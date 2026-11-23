// SPDX-License-Identifier: FSL-1.1-ALv2

package api

import (
	"context"
	"errors"
	"sync"
)

// errManualImportExpiry is the cause that ManualImportExpiry.Expire gives the
// context of the import flow.
var errManualImportExpiry = errors.New("import flow expired by the test")

// ManualImportExpiry gives a test control over the expiry of the context that
// bounds the import flow (the import command and the create send that follows
// it) in place of the Timing.ImportWait deadline, so a test does not depend
// on how long its own setup takes. The context stays live until Expire; it
// then reports context.DeadlineExceeded like an expired deadline does, and a
// close of the agent channel still ends it with the channel's cause.
type ManualImportExpiry struct {
	s       *Service
	cancel  context.CancelCauseFunc
	pending func() string

	mu      sync.Mutex
	entered chan struct{}
	closed  bool
}

// UseManualImportExpiryForTest replaces the import deadline of every later
// import flow of the service with a context that only the returned handle
// expires. sandboxID returns the ID of the sandbox whose create send the test
// observes, or an empty string while it is not known.
func (s *Service) UseManualImportExpiryForTest(sandboxID func() string) *ManualImportExpiry {
	m := &ManualImportExpiry{s: s, pending: sandboxID, entered: make(chan struct{})}
	hook := func(parent context.Context) (context.Context, context.CancelFunc) {
		ctx, cancel := context.WithCancelCause(parent)
		m.mu.Lock()
		m.cancel = cancel
		m.mu.Unlock()
		return &manualExpiryContext{Context: ctx, m: m}, func() { cancel(context.Canceled) }
	}
	s.importContext.Store(&hook)
	return m
}

// Expire ends the import flow context as an expired deadline does.
func (m *ManualImportExpiry) Expire() {
	m.mu.Lock()
	cancel := m.cancel
	m.mu.Unlock()
	cancel(errManualImportExpiry)
}

// CreateSendWaiting is closed when the create command of the observed
// sandbox is registered and its send waits on the context of the import flow
// (Conn.Send selects on the context only after the enqueue failed and the
// queue-full wait started). Nothing else selects on the context between the
// registration of the create command and the end of its send, so the first
// such call after the registration is that wait.
func (m *ManualImportExpiry) CreateSendWaiting() <-chan struct{} { return m.entered }

func (m *ManualImportExpiry) doneRequested() {
	id := m.pending()
	if id == "" || !m.s.commands.hasPending(cmdCreateSandbox, id) {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.closed {
		m.closed = true
		close(m.entered)
	}
}

// manualExpiryContext reports DeadlineExceeded once the test expired it.
type manualExpiryContext struct {
	context.Context
	m *ManualImportExpiry
}

func (c *manualExpiryContext) Done() <-chan struct{} {
	c.m.doneRequested()
	return c.Context.Done()
}

func (c *manualExpiryContext) Err() error {
	err := c.Context.Err()
	if err != nil && errors.Is(context.Cause(c.Context), errManualImportExpiry) {
		return context.DeadlineExceeded
	}
	return err
}

// hasPending reports whether a command of kind for sandboxID has no result
// yet.
func (c *commandStore) hasPending(kind commandKind, sandboxID string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, p := range c.m {
		if p.info.kind == kind && p.info.sandboxID == sandboxID {
			return true
		}
	}
	return false
}
