// SPDX-License-Identifier: Apache-2.0

package datapath

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// Credit windows count bytes of encoded DataFrame messages (FrameCost),
// without the data channel length prefix. Only payload frames use credit.

// MaxWindowSize bounds a credit window, so that a faulty peer cannot
// overflow the counters with window updates.
const MaxWindowSize int64 = 1 << 40

// ErrWindowExceeded reports a peer that sent payload frames beyond the
// credit window that it was granted. It breaks the protocol: the control
// plane closes the agent relay connection, and the agent ends the stream.
var ErrWindowExceeded = errors.New("datapath: frame exceeds the granted credit window")

// ErrInvalidWindowUpdate reports a window update that is not positive or
// that would grow a window beyond MaxWindowSize.
var ErrInvalidWindowUpdate = errors.New("datapath: invalid window update")

// SendWindow tracks the credit that the receiver of one stream granted to
// this sender. The sender consumes credit before it sends a payload frame
// and adds the increment of each window update it receives. It is safe for
// concurrent use.
type SendWindow struct {
	mu        sync.Mutex
	available int64
	changed   chan struct{}
}

// NewSendWindow returns a send window with the initial credit that the
// receiver granted (for example RelayStreamOpen.initial_window_bytes).
func NewSendWindow(initial int64) (*SendWindow, error) {
	if initial < 0 || initial > MaxWindowSize {
		return nil, fmt.Errorf("%w: initial window %d", ErrInvalidWindowUpdate, initial)
	}
	return &SendWindow{available: initial, changed: make(chan struct{})}, nil
}

// Available returns the credit that the sender may still use.
func (w *SendWindow) Available() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.available
}

// TryConsume uses n bytes of credit when that much is available and reports
// whether it did.
func (w *SendWindow) TryConsume(n int64) bool {
	if n < 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if n > w.available {
		return false
	}
	w.available -= n
	return true
}

// Consume waits until n bytes of credit are available and uses them. It
// returns the context error when ctx ends first. A frame never needs more
// than MaxFrameSize bytes, and every window starts at MinWindowSize or more,
// so a consumer whose peer returns freed credit is never blocked forever.
func (w *SendWindow) Consume(ctx context.Context, n int64) error {
	if n < 0 || n > MaxWindowSize {
		return fmt.Errorf("datapath: invalid credit request %d", n)
	}
	for {
		w.mu.Lock()
		if n <= w.available {
			w.available -= n
			w.mu.Unlock()
			return nil
		}
		changed := w.changed
		w.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// Add adds the increment of a window update to the window and wakes
// waiting consumers.
func (w *SendWindow) Add(increment int64) error {
	if increment <= 0 {
		return fmt.Errorf("%w: increment %d", ErrInvalidWindowUpdate, increment)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.available+increment > MaxWindowSize {
		return fmt.Errorf("%w: window would exceed %d bytes", ErrInvalidWindowUpdate, MaxWindowSize)
	}
	w.available += increment
	close(w.changed)
	w.changed = make(chan struct{})
	return nil
}

// ReceiveWindow tracks one stream's receive buffer on the receiving side. The
// receiver grants a window equal to its buffer capacity, accounts each
// received payload frame against the outstanding credit and, when its
// consumer frees buffered bytes, returns exactly those bytes to the sender
// with a window update. Outstanding credit plus buffered bytes always equal
// the capacity, so the receiver never buffers more than its capacity. It is
// safe for concurrent use.
type ReceiveWindow struct {
	mu          sync.Mutex
	capacity    int64
	outstanding int64
	buffered    int64
}

// NewReceiveWindow returns a receive window whose buffer holds capacity
// bytes. The capacity is at least MinWindowSize.
func NewReceiveWindow(capacity int64) (*ReceiveWindow, error) {
	if capacity < MinWindowSize || capacity > MaxWindowSize {
		return nil, fmt.Errorf("datapath: receive window %d outside [%d, %d]", capacity, MinWindowSize, MaxWindowSize)
	}
	return &ReceiveWindow{capacity: capacity, outstanding: capacity}, nil
}

// InitialWindow returns the first window that the receiver grants: its
// buffer capacity.
func (w *ReceiveWindow) InitialWindow() int64 {
	return w.capacity
}

// Receive accounts a received frame of cost bytes (FrameCost). It returns
// ErrWindowExceeded when the sender had less credit than cost; the frame
// must then not be buffered.
func (w *ReceiveWindow) Receive(cost int64) error {
	if cost < 0 {
		return fmt.Errorf("datapath: invalid frame cost %d", cost)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if cost > w.outstanding {
		return fmt.Errorf("%w: cost %d, outstanding credit %d", ErrWindowExceeded, cost, w.outstanding)
	}
	w.outstanding -= cost
	w.buffered += cost
	return nil
}

// Release accounts bytes that the consumer freed and returns the increment
// of the window update to send, which equals freed. A zero increment needs
// no window update.
func (w *ReceiveWindow) Release(freed int64) (int64, error) {
	if freed < 0 {
		return 0, fmt.Errorf("datapath: invalid release %d", freed)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if freed > w.buffered {
		return 0, fmt.Errorf("datapath: release %d exceeds buffered %d", freed, w.buffered)
	}
	w.buffered -= freed
	w.outstanding += freed
	return freed, nil
}

// Outstanding returns the credit that the sender still holds.
func (w *ReceiveWindow) Outstanding() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.outstanding
}

// Buffered returns the received bytes that the consumer has not freed.
func (w *ReceiveWindow) Buffered() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buffered
}
