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
	"time"

	"github.com/delinoio/mono/go/uuidv7"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// outboundQueue is the number of envelopes that may wait for the writer of
// one channel. A full queue means the agent does not read.
const outboundQueue = 256

// DefaultQueueFullWait is how long Send waits for room in a full outbound
// queue before it declares the channel saturated and closes it. A caller
// can legitimately queue more than outboundQueue envelopes in a burst (for
// example the delete commands after a state report), so the wait gives the
// writer time to drain; a stalled agent does not drain and is cut off
// after the wait instead of blocking the caller for ever.
const DefaultQueueFullWait = 5 * time.Second

// ErrNoChannel reports a device without a current agent channel on this
// instance.
var ErrNoChannel = errors.New("the device has no agent channel on this instance")

// ErrQueueFull is the close cause of a channel whose outbound queue stayed
// full for the queue-full wait: the agent does not read its channel.
var ErrQueueFull = errors.New("the agent channel closed because its outbound queue stayed full")

// ErrChannelClosed reports a channel that closed while a caller used it.
var ErrChannelClosed = errors.New("the agent channel closed")

// Conn is one agent channel.
type Conn struct {
	DeviceID       string
	OrganizationID string
	// Generation increases with every channel the hub registers. It is a
	// process-local counter that restarts with the process; use ID to
	// identify a channel across restarts.
	Generation uint64
	// ID is a UUID v7 that is unique to this channel across control plane
	// restarts. Durable records (for example the owner of a snapshot
	// export) store it to tell later channels from the one they belong to.
	ID string

	out    chan *ownsandv1.AgentEnvelope
	ctx    context.Context
	cancel context.CancelCauseFunc
	// queueFullWait is the bounded wait of Send on a full queue.
	queueFullWait time.Duration
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
	// configMu serializes the configuration sends of the channel, from
	// reading the device limits to queueing the message; see LockConfig.
	configMu sync.Mutex
	// configWaiters counts the senders that are blocked in LockConfig; see
	// ConfigLockWaiters.
	configWaiters atomic.Int32
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
// ended first. When the outbound queue is full, Send waits for the queue
// full wait for room; if the writer frees none, the agent does not read, so
// Send closes the channel with ErrQueueFull and returns that error. The
// close takes the normal disconnect path of the channel handler, so the
// caller (the channel handler or the shared control event worker) is never
// blocked by a stalled agent beyond that wait.
func (c *Conn) Send(ctx context.Context, env *ownsandv1.AgentEnvelope) error {
	select {
	case <-c.ctx.Done():
		return ErrChannelClosed
	default:
	}
	// A caller whose context already ended gets its error even when the
	// queue has room, so the envelope is never queued for a request that
	// stopped waiting for the result. select picks among ready cases at
	// random, so the enqueue below needs this explicit check.
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case c.out <- env:
		return nil
	default:
	}
	timer := time.NewTimer(c.queueFullWait)
	defer timer.Stop()
	select {
	case c.out <- env:
		return nil
	case <-c.ctx.Done():
		return ErrChannelClosed
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		// Close keeps the first cause, so an earlier close reason wins.
		c.Close(ErrQueueFull)
		return ErrQueueFull
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

// LockConfig takes the per-channel configuration lock and returns its release
// function. The channel handler (when the channel opens) and the control event
// processor (after a change of device limits) both read the limits and queue
// a configuration message for the same channel. Without one lock, a send that
// read older limits can queue its message after the message of a newer
// send, and the agent then keeps the older limits. Hold the lock from reading
// the limits until the message is queued, so the sender that runs later also
// reads the newer committed limits.
func (c *Conn) LockConfig() (unlock func()) {
	c.configWaiters.Add(1)
	c.configMu.Lock()
	c.configWaiters.Add(-1)
	return c.configMu.Unlock
}

// ConfigLockWaiters reports the number of senders that wait in LockConfig for
// the holder to release the lock. A sync.Mutex cannot report its waiters, so a
// test uses this count to see that a second sender reached the lock instead of
// sleeping for an assumed time. Production code does not read it.
func (c *Conn) ConfigLockWaiters() int { return int(c.configWaiters.Load()) }

// Hub is the registry of agent channels.
type Hub struct {
	mu    sync.Mutex
	conns map[string]*Conn
	gen   uint64
	// listeners run when a channel closes (for example to end the relay
	// connections and grants bound to it).
	closeListeners []func(*Conn)
	// QueueFullWait is the bounded wait of Send on a full outbound queue
	// for channels registered later; tests shorten it. Set it before the
	// first Register.
	QueueFullWait time.Duration
	// fences holds the per-device fences that are in use (see lockDevice).
	fences map[string]*deviceFence

	// Commands holds the waiters for command results by command ID.
	Commands *Waiters[string, *ownsandv1.AgentCommandResult]
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{conns: map[string]*Conn{}, fences: map[string]*deviceFence{}, QueueFullWait: DefaultQueueFullWait, Commands: NewWaiters[string, *ownsandv1.AgentCommandResult]()}
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
	unlock := h.lockDevice(deviceID)
	defer unlock()
	return h.register(parent, deviceID, orgID)
}

// RegisterAdmitted registers a new current channel like Register, but only
// when admit runs its argument. It takes the device fence first and calls
// admit with the fence held; admit decides whether to run the registration
// (it returns false when it refused) and RegisterAdmitted returns nil then.
// The registration runs inside admit, so admit can make the ownership
// check and the registration one step under its own lock (the lease's
// ownership lock). Because the fence is taken before admit, that lock is
// never held while the call waits for the fence: the fence is held across
// database I/O by IfNoChannel (the offline write of a closing channel), and
// a wait for it under the ownership lock would stall every admission, the
// lease renewal and the loss transition behind that I/O.
//
// Lock order: the device fence, then the lock of admit, then the hub lock.
// admit and the function it runs must not take a fence.
func (h *Hub) RegisterAdmitted(parent context.Context, deviceID, orgID string, admit func(register func()) bool) *Conn {
	unlock := h.lockDevice(deviceID)
	defer unlock()
	var c *Conn
	if !admit(func() { c = h.register(parent, deviceID, orgID) }) {
		return nil
	}
	return c
}

// register adds the channel. The caller holds the device fence.
func (h *Hub) register(parent context.Context, deviceID, orgID string) *Conn {
	ctx, cancel := context.WithCancelCause(parent)
	// crypto/rand does not fail on supported platforms.
	id, err := uuidv7.NewString()
	if err != nil {
		panic(err)
	}
	h.mu.Lock()
	h.gen++
	c := &Conn{DeviceID: deviceID, OrganizationID: orgID, Generation: h.gen, ID: id, out: make(chan *ownsandv1.AgentEnvelope, outboundQueue), ctx: ctx, cancel: cancel, queueFullWait: h.QueueFullWait}
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
