// SPDX-License-Identifier: FSL-1.1-ALv2

// Package hub keeps the agent channels of the lease holder in memory: one
// current channel per device with its generation, the outbound queue of
// each channel and the waiters for agent answers (command results and,
// later, signaling answers and relay grant acknowledgements). The owning
// contract is the Agent channel section of
// docs/servers-ownsand-api-foundation.md.
package hub

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// outboundQueue is the number of envelopes that may wait for the writer of
// one channel. A full queue means the agent does not read; Send then waits
// until the context ends.
const outboundQueue = 256

// ErrNoChannel reports a device without a current agent channel on this
// instance.
var ErrNoChannel = errors.New("the device has no agent channel on this instance")

// ErrChannelClosed reports a channel that closed while a caller used it.
var ErrChannelClosed = errors.New("the agent channel closed")

// Conn is one agent channel.
type Conn struct {
	DeviceID       string
	OrganizationID string
	// Generation increases with every channel the hub registers.
	Generation uint64

	out    chan *ownsandv1.AgentEnvelope
	ctx    context.Context
	cancel context.CancelCauseFunc
	// reconciled is set when the server applied the reconnection state
	// report; only then is the device online and eligible.
	reconciled atomic.Bool

	mu sync.Mutex
	// values holds per-channel state of higher layers (for example the
	// last update target sent).
	values map[any]any

	// updateMu serializes the update decisions of the channel, from reading
	// the policy to queueing the command; see LockUpdates.
	updateMu sync.Mutex
}

// Context ends when the channel closes.
func (c *Conn) Context() context.Context { return c.ctx }

// Done is closed when the channel closes.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// Close closes the channel with cause.
func (c *Conn) Close(cause error) { c.cancel(cause) }

// Outbound returns the queue that the channel writer drains.
func (c *Conn) Outbound() <-chan *ownsandv1.AgentEnvelope { return c.out }

// Reconciled reports whether the state report was applied.
func (c *Conn) Reconciled() bool { return c.reconciled.Load() }

// SetReconciled marks the state report as applied.
func (c *Conn) SetReconciled() { c.reconciled.Store(true) }

// Send queues env for the agent. It fails when the channel closed or ctx
// ended first.
func (c *Conn) Send(ctx context.Context, env *ownsandv1.AgentEnvelope) error {
	select {
	case <-c.ctx.Done():
		return ErrChannelClosed
	default:
	}
	select {
	case c.out <- env:
		return nil
	case <-c.ctx.Done():
		return ErrChannelClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Value returns a per-channel value.
func (c *Conn) Value(key any) any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.values[key]
}

// SetValue stores a per-channel value.
func (c *Conn) SetValue(key, value any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.values == nil {
		c.values = map[any]any{}
	}
	c.values[key] = value
}

// LockUpdates takes the per-channel update lock and returns its release
// function. The channel handler and the control event processor both decide
// on updates for the same channel. Without one lock, a decision that read an
// older policy can queue its command after the command of a newer decision,
// and the agent then applies the older decision last. Hold the lock from
// reading the policy until the command is queued.
func (c *Conn) LockUpdates() (unlock func()) {
	c.updateMu.Lock()
	return c.updateMu.Unlock
}

// Hub is the registry of agent channels.
type Hub struct {
	mu    sync.Mutex
	conns map[string]*Conn
	gen   uint64
	// listeners run when a channel closes (for example to end the relay
	// connections and grants bound to it).
	closeListeners []func(*Conn)
	// fences holds the per-device fences that are in use (see lockDevice).
	fences map[string]*deviceFence

	// Commands holds the waiters for command results by command ID.
	Commands *Waiters[string, *ownsandv1.AgentCommandResult]
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{conns: map[string]*Conn{}, fences: map[string]*deviceFence{}, Commands: NewWaiters[string, *ownsandv1.AgentCommandResult]()}
}

// deviceFence serializes the registration of a new channel of one device
// with the offline write of a closing channel of the same device. refs
// counts the goroutines that hold or wait for mu, so that the hub drops the
// fence of an idle device.
type deviceFence struct {
	mu   sync.Mutex
	refs int
}

// lockDevice takes the fence of the device and returns its release
// function. The fence is an in-process lock: it never spans an instance, and
// the lease holder is the only instance that has channels.
//
// Lock order: a caller that also locks the device row in Postgres locks the
// row first and the fence second. Register holds no row lock while it waits
// for the fence, so a holder of the row lock that waits for the fence waits
// only for a Register call that needs no further lock.
func (h *Hub) lockDevice(deviceID string) (unlock func()) {
	h.mu.Lock()
	f := h.fences[deviceID]
	if f == nil {
		f = &deviceFence{}
		h.fences[deviceID] = f
	}
	f.refs++
	h.mu.Unlock()
	f.mu.Lock()
	return func() {
		f.mu.Unlock()
		h.mu.Lock()
		f.refs--
		if f.refs == 0 {
			delete(h.fences, deviceID)
		}
		h.mu.Unlock()
	}
}

// OnClose adds a listener that runs after a channel closed and was removed.
func (h *Hub) OnClose(fn func(*Conn)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeListeners = append(h.closeListeners, fn)
}

// Register adds a new current channel for the device. It closes the
// previous channel of the device, whose later events no longer change the
// device state. Register waits for an IfNoChannel call of the same device
// that is running, so a new channel is never registered between the check
// and the write of that call.
func (h *Hub) Register(parent context.Context, deviceID, orgID string) *Conn {
	ctx, cancel := context.WithCancelCause(parent)
	unlock := h.lockDevice(deviceID)
	defer unlock()
	h.mu.Lock()
	h.gen++
	c := &Conn{DeviceID: deviceID, OrganizationID: orgID, Generation: h.gen, out: make(chan *ownsandv1.AgentEnvelope, outboundQueue), ctx: ctx, cancel: cancel}
	old := h.conns[deviceID]
	h.conns[deviceID] = c
	h.mu.Unlock()
	if old != nil {
		old.Close(errors.New("replaced by a newer agent channel"))
	}
	return c
}

// Unregister removes c and reports whether it was the current channel of
// its device. Only the current channel's close changes the device state.
func (h *Hub) Unregister(c *Conn) bool {
	c.Close(ErrChannelClosed)
	h.mu.Lock()
	current := h.conns[c.DeviceID] == c
	if current {
		delete(h.conns, c.DeviceID)
	}
	listeners := append([]func(*Conn){}, h.closeListeners...)
	h.mu.Unlock()
	for _, fn := range listeners {
		fn(c)
	}
	return current
}

// IfNoChannel runs fn when the device has no registered channel, and
// reports whether it ran. fn runs under the device fence: Register of the
// same device waits until fn returns, and no channel of the device is
// registered while fn runs. A channel that closed and was unregistered uses
// it for the offline write, so that the write cannot take a device offline
// that a replacement channel registered after the unregistration (the
// replacement's state report then sets it online after the write, not
// before). It runs outside the close listeners, which run in Unregister
// without a lock. fn must not call Register of the same device.
func (h *Hub) IfNoChannel(deviceID string, fn func()) bool {
	unlock := h.lockDevice(deviceID)
	defer unlock()
	h.mu.Lock()
	registered := h.conns[deviceID] != nil
	h.mu.Unlock()
	if registered {
		return false
	}
	fn()
	return true
}

// Get returns the current channel of the device, or nil.
func (h *Hub) Get(deviceID string) *Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	c := h.conns[deviceID]
	if c == nil {
		return nil
	}
	select {
	case <-c.Done():
		return nil
	default:
		return c
	}
}

// Live returns the current channel when the device's state report was
// applied, or nil.
func (h *Hub) Live(deviceID string) *Conn {
	c := h.Get(deviceID)
	if c == nil || !c.Reconciled() {
		return nil
	}
	return c
}

// All returns every current channel.
func (h *Hub) All() []*Conn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*Conn, 0, len(h.conns))
	for _, c := range h.conns {
		out = append(out, c)
	}
	return out
}

// CloseAll closes every channel, for example when the instance loses or
// releases the lease.
func (h *Hub) CloseAll(cause error) {
	for _, c := range h.All() {
		c.Close(cause)
	}
}

// Waiters delivers one value per key to the goroutine that waits for it.
type Waiters[K comparable, V any] struct {
	mu sync.Mutex
	m  map[K]chan V
}

// NewWaiters returns an empty waiter set.
func NewWaiters[K comparable, V any]() *Waiters[K, V] {
	return &Waiters[K, V]{m: map[K]chan V{}}
}

// Register returns the channel that receives the value of key. Call Cancel
// when the waiter gives up.
func (w *Waiters[K, V]) Register(key K) <-chan V {
	ch := make(chan V, 1)
	w.mu.Lock()
	w.m[key] = ch
	w.mu.Unlock()
	return ch
}

// Deliver hands v to the waiter of key and reports whether one existed.
func (w *Waiters[K, V]) Deliver(key K, v V) bool {
	w.mu.Lock()
	ch, ok := w.m[key]
	delete(w.m, key)
	w.mu.Unlock()
	if ok {
		ch <- v
	}
	return ok
}

// Cancel removes the waiter of key.
func (w *Waiters[K, V]) Cancel(key K) {
	w.mu.Lock()
	delete(w.m, key)
	w.mu.Unlock()
}
