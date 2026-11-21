// SPDX-License-Identifier: FSL-1.1-ALv2

// Package relay holds the agent relay connections of the lease holder
// (RelayAgentService.Connect) and the relay streams that the RelayService
// RPCs open on them: stream_id allocation, frame routing, credit windows in
// both directions, keepalives, the idle timeout and the supported frame
// kinds. Frames stay in process memory; nothing here logs, persists or
// inspects sandbox content. The owning contracts are the Relay section of
// docs/servers-ownsand-api-foundation.md and the data path framing of
// docs/protos-ownsand-api-contract.md.
package relay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/delinoio/mono/go/ownsand/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
)

// Limits are the relay limits (recorded in the contract).
type Limits struct {
	// StreamBuffer is the per-stream buffer toward the caller and the first
	// credit window granted to the agent (at least one maximum frame).
	StreamBuffer int64
	// MaxStreamsPerConn bounds the open streams of one agent connection.
	MaxStreamsPerConn int
	// MaxStreamsPerGrant bounds the open streams of one grant.
	MaxStreamsPerGrant int
	// Keepalive is the outbound idle time after which a keepalive frame is
	// sent.
	Keepalive time.Duration
	// IdleTimeout closes a connection that received nothing.
	IdleTimeout time.Duration
	// MaxQueuedControlFrames bounds the frames that carry no credit (request
	// accepted, session end, error, stream close and stream open frames) that
	// wait in one stream queue for the handler. Zero selects the default.
	MaxQueuedControlFrames int
	// MaxQueuedControlBytes bounds the encoded bytes of those frames in one
	// stream queue. A frame without credit can be as large as a maximum frame,
	// so the count alone does not bound the memory. Zero selects the default.
	MaxQueuedControlBytes int64
}

const (
	defaultMaxQueuedControlFrames       = 4
	defaultMaxQueuedControlBytes  int64 = 64 << 10
)

// DefaultLimits returns the recorded limits.
func DefaultLimits() Limits {
	return Limits{
		StreamBuffer:       4 << 20,
		MaxStreamsPerConn:  64,
		MaxStreamsPerGrant: 4,
		Keepalive:          30 * time.Second,
		IdleTimeout:        90 * time.Second,

		MaxQueuedControlFrames: defaultMaxQueuedControlFrames,
		MaxQueuedControlBytes:  defaultMaxQueuedControlBytes,
	}
}

// writerGrace is how long Conn.Run waits for the writer after the connection
// closed. A send that makes progress finishes within it; a send blocked on
// the peer's flow control does not, and Run returns without it.
const writerGrace = 100 * time.Millisecond

// outboundQueue bounds the frames waiting for the connection writer.
const outboundQueue = 1024

// Errors.
var (
	ErrConnClosed     = errors.New("the agent relay connection closed")
	ErrReplaced       = errors.New("a newer agent relay connection replaced this one")
	ErrTooManyStreams = errors.New("too many relay streams")
	ErrWindowExceeded = errors.New("the agent sent beyond its credit window")
	ErrControlFlood   = errors.New("the agent queued too many frames without credit on one stream")
	ErrStreamClosed   = errors.New("the relay stream closed")
	ErrIdle           = errors.New("no byte received for the idle timeout")
	ErrUnknownKind    = errors.New("unknown frame kind on a stream")
	errMalformedFrame = errors.New("malformed frame")
	firstVersionKinds = datapath.AllKinds()
)

// Conn is one agent relay connection.
type Conn struct {
	DeviceID string
	// Generation is the agent channel generation the connection is bound to.
	Generation uint64
	limits     Limits

	out    chan *ownsandv1.DataFrame
	ctx    context.Context
	cancel context.CancelCauseFunc

	// sendInterrupt is set by SetSendInterrupt before Run starts.
	sendInterrupt func() error

	lastRecv atomic.Int64
	lastSend atomic.Int64
	// lastLarge is when the connection last received a large frame of an
	// open stream (zero before the first one); see LargeFrameBytes.
	lastLarge atomic.Int64

	// openGate serializes Open: a holder allocates a stream_id and enqueues
	// the stream open frame before the next Open starts, so the stream open
	// frames reach the agent in increasing stream_id order. It is a channel
	// and not a mutex so that a waiting Open honors its context and the
	// connection closing.
	openGate chan struct{}

	mu        sync.Mutex
	next      uint64
	streams   map[uint64]*Stream
	perGrant  map[string]int
	peerKinds map[ownsandv1.DataFrameKind]bool
}

// NewConn returns a connection for the device bound to an agent channel
// generation.
func NewConn(parent context.Context, deviceID string, gen uint64, limits Limits) *Conn {
	if limits.MaxQueuedControlFrames <= 0 {
		limits.MaxQueuedControlFrames = defaultMaxQueuedControlFrames
	}
	if limits.MaxQueuedControlBytes <= 0 {
		limits.MaxQueuedControlBytes = defaultMaxQueuedControlBytes
	}
	ctx, cancel := context.WithCancelCause(parent)
	c := &Conn{DeviceID: deviceID, Generation: gen, limits: limits, out: make(chan *ownsandv1.DataFrame, outboundQueue),
		ctx: ctx, cancel: cancel, openGate: make(chan struct{}, 1), streams: map[uint64]*Stream{}, perGrant: map[string]int{}}
	now := time.Now().UnixNano()
	c.lastRecv.Store(now)
	c.lastSend.Store(now)
	return c
}

// SetSendInterrupt registers the function that makes a blocked send fail
// (for an HTTP response, a write deadline in the past). Run calls it once
// after the connection closed. It returns an error when it cannot interrupt
// the send; Run then falls back to a bounded wait for the writer. Call it
// before Run.
func (c *Conn) SetSendInterrupt(interrupt func() error) { c.sendInterrupt = interrupt }

// Done is closed when the connection closes.
func (c *Conn) Done() <-chan struct{} { return c.ctx.Done() }

// Err returns why the connection closed.
func (c *Conn) Err() error { return context.Cause(c.ctx) }

// Close closes the connection and fails its streams.
func (c *Conn) Close(cause error) {
	c.cancel(cause)
	c.mu.Lock()
	streams := c.streams
	c.streams = map[uint64]*Stream{}
	c.mu.Unlock()
	for _, s := range streams {
		s.fail(cause)
	}
}

// StreamCount returns the open streams (tests and logs).
func (c *Conn) StreamCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.streams)
}

// LastReceive returns when the connection last received a frame.
func (c *Conn) LastReceive() time.Time { return time.Unix(0, c.lastRecv.Load()) }

// LargeFrameBytes is the encoded size from which a received stream frame
// counts as large. The control plane cannot see a frame in transit, because
// Connect delivers whole frames, so only a large frame, which can still have
// been arriving when the agent's request accepted notice was ready, extends
// the wait for that notice.
const LargeFrameBytes = 64 << 10

// LastLargeReceive returns when the connection last received a frame of an
// open stream that is at least LargeFrameBytes large, or the zero time when
// it received none. Keepalives, other connection-level frames, frames of a
// closed stream and small frames do not count.
func (c *Conn) LastLargeReceive() time.Time {
	n := c.lastLarge.Load()
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
}

func (c *Conn) enqueue(ctx context.Context, f *ownsandv1.DataFrame) error {
	select {
	case c.out <- f:
		return nil
	case <-c.ctx.Done():
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Supports reports whether the agent listed kind (or, without a list, the
// kind belongs to the first version).
func (c *Conn) Supports(kind ownsandv1.DataFrameKind) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.peerKinds == nil {
		return datapath.IsKnownKind(kind)
	}
	return c.peerKinds[kind]
}

// Run serves the connection: it sends the supported frame kinds, writes
// queued frames with send, reads frames with recv, sends keepalives and
// closes an idle connection. It returns when the connection closes.
//
// The idle check runs in its own goroutine because send can block on the
// peer's flow control: a peer that stops reading and sends nothing would
// otherwise stall the writer, and with it the idle check, forever. For the
// same reason Run waits for the writer only for writerGrace, enough for a
// send that makes progress to finish before the caller ends the underlying
// stream, and returns without a send that is still blocked. Returning lets
// the caller end the stream, which unblocks that send.
//
// That fallback is unsafe when the send writes to an HTTP response: the
// handler that returns then finishes the response while the send still
// writes to it. A caller whose send can be interrupted registers the
// interrupt with SetSendInterrupt. Run then calls it once the connection
// closed and waits for the writer without a limit, so Run returns only after
// the last send ended.
func (c *Conn) Run(recv func() (*ownsandv1.DataFrame, error), send func(*ownsandv1.DataFrame) error) error {
	// guardedSend skips send once the connection closed, so the writer does
	// not start a send after Run returned. A send that is already blocked
	// ends when the caller ends the underlying stream.
	guardedSend := func(f *ownsandv1.DataFrame) error {
		if c.ctx.Err() != nil {
			return context.Cause(c.ctx)
		}
		return send(f)
	}
	tick := c.limits.Keepalive / 3
	go func() {
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if time.Since(time.Unix(0, c.lastRecv.Load())) > c.limits.IdleTimeout {
					c.Close(ErrIdle)
					return
				}
			case <-c.ctx.Done():
				return
			}
		}
	}()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		ticker := time.NewTicker(tick)
		defer ticker.Stop()
		for {
			select {
			case f := <-c.out:
				if err := guardedSend(f); err != nil {
					c.Close(err)
					return
				}
				c.lastSend.Store(time.Now().UnixNano())
			case <-ticker.C:
				now := time.Now()
				if now.Sub(time.Unix(0, c.lastSend.Load())) >= c.limits.Keepalive {
					if err := guardedSend(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})); err != nil {
						c.Close(err)
						return
					}
					c.lastSend.Store(now.UnixNano())
				}
			case <-c.ctx.Done():
				return
			}
		}
	}()
	_ = c.enqueue(c.ctx, datapath.MustNewFrame(0, "", &ownsandv1.SupportedFrameKinds{Kinds: datapath.AllKinds()}))
	go func() {
		for {
			f, err := recv()
			if err != nil {
				c.Close(err)
				return
			}
			c.lastRecv.Store(time.Now().UnixNano())
			if err := c.dispatch(f); err != nil {
				c.Close(err)
				return
			}
		}
	}()
	<-c.ctx.Done()
	if c.sendInterrupt != nil && c.sendInterrupt() == nil {
		// The interrupt makes a blocked send fail, so the writer ends soon
		// and Run can wait for it without a limit. The caller owns the
		// stream again only after the writer stopped using it.
		<-writerDone
		return c.Err()
	}
	grace := time.NewTimer(writerGrace)
	defer grace.Stop()
	select {
	case <-writerDone:
	case <-grace.C:
	}
	return c.Err()
}

func (c *Conn) dispatch(f *ownsandv1.DataFrame) error {
	size := datapath.EncodedSize(f)
	if size > datapath.MaxFrameSize {
		return errMalformedFrame
	}
	if f.GetStreamId() == 0 {
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_RelayKeepalive:
			if p.RelayKeepalive.GetEchoRequested() {
				return c.enqueue(c.ctx, datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{}))
			}
		case *ownsandv1.DataFrame_SupportedFrameKinds:
			kinds := map[ownsandv1.DataFrameKind]bool{}
			for _, k := range p.SupportedFrameKinds.GetKinds() {
				kinds[k] = true
			}
			if len(kinds) == 0 {
				for _, k := range firstVersionKinds {
					kinds[k] = true
				}
			}
			c.mu.Lock()
			c.peerKinds = kinds
			c.mu.Unlock()
		}
		// Unknown kinds on stream 0 are ignored.
		return nil
	}
	c.mu.Lock()
	s := c.streams[f.GetStreamId()]
	c.mu.Unlock()
	if s == nil {
		// A late frame of a closed stream is dropped.
		return nil
	}
	if size >= LargeFrameBytes {
		c.lastLarge.Store(time.Now().UnixNano())
	}
	if err := datapath.ValidateFrame(f); err != nil {
		var unknown *datapath.UnknownKindError
		if errors.As(err, &unknown) {
			// End the stream with an error frame, not a stream close frame:
			// the agent ends the stream on it without a reply. The stream
			// closes before it fails, so the RPC, which wakes on the failure,
			// finds it closed and sends no stream close frame behind the
			// error frame.
			s.closeWith(datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.ErrorDetail{
				Code:    ownsandv1.ErrorCode_ERROR_CODE_UNIMPLEMENTED,
				Message: fmt.Sprintf("unknown frame kind %d on stream %d", int32(unknown.Kind), unknown.StreamID),
			}))
			s.fail(ErrUnknownKind)
			return nil
		}
		return errMalformedFrame
	}
	if win, ok := f.GetPayload().(*ownsandv1.DataFrame_RelayWindowUpdate); ok {
		if err := s.send.Add(win.RelayWindowUpdate.GetIncrementBytes()); err != nil {
			return err
		}
		return nil
	}
	if datapath.ConsumesCredit(f.GetKind()) {
		if err := s.recv.Receive(datapath.FrameCost(f)); err != nil {
			return ErrWindowExceeded
		}
	}
	return s.push(f)
}

// Stream is one relay stream.
type Stream struct {
	ID        uint64
	GrantID   string
	SandboxID string
	conn      *Conn
	recv      *datapath.ReceiveWindow
	send      *datapath.SendWindow

	mu    sync.Mutex
	queue []*ownsandv1.DataFrame
	// controlFrames and controlBytes count the queued frames that carry no
	// credit. The receive credit window bounds every other queued frame.
	controlFrames int
	controlBytes  int64
	notify        chan struct{}
	err           error
	closed        bool
	// Started is when the stream_id was allocated.
	Started time.Time
}

// Open allocates a new stream_id (increasing from 1, never reused) and
// sends the stream open frame with the first credit window.
//
// The agent treats the highest stream_id that it has seen as its replay
// protection and drops a stream open frame whose stream_id is not above it,
// without a reply. Open therefore holds the connection's open gate from the
// stream_id allocation until the stream open frame is in the outbound queue
// (the single writer preserves the queue order), so that concurrent opens
// reach the agent in increasing stream_id order. An Open that waits for the
// gate, or for room in a full outbound queue, returns when ctx ends or the
// connection closes; it then holds no stream slot, and the stream_id of an
// Open that allocated one is not reused. Only Open waits on the gate: frames
// of existing streams, including a sender that waits for the agent's
// credit, never do.
func (c *Conn) Open(ctx context.Context, grantID, sandboxID string) (*Stream, error) {
	recv, err := datapath.NewReceiveWindow(c.limits.StreamBuffer)
	if err != nil {
		return nil, err
	}
	send, _ := datapath.NewSendWindow(0)
	select {
	case c.openGate <- struct{}{}:
	case <-c.ctx.Done():
		return nil, ErrConnClosed
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-c.openGate }()
	c.mu.Lock()
	select {
	case <-c.ctx.Done():
		c.mu.Unlock()
		return nil, ErrConnClosed
	default:
	}
	if len(c.streams) >= c.limits.MaxStreamsPerConn || c.perGrant[grantID] >= c.limits.MaxStreamsPerGrant {
		c.mu.Unlock()
		return nil, ErrTooManyStreams
	}
	c.next++
	s := &Stream{ID: c.next, GrantID: grantID, SandboxID: sandboxID, conn: c, recv: recv, send: send, notify: make(chan struct{}), Started: time.Now()}
	c.streams[s.ID] = s
	c.perGrant[grantID]++
	c.mu.Unlock()
	if err := c.enqueue(ctx, datapath.MustNewFrame(s.ID, sandboxID, &ownsandv1.RelayStreamOpen{GrantId: grantID, InitialWindowBytes: recv.InitialWindow()})); err != nil {
		// The agent never saw the stream open frame, so no stream close
		// frame is sent; the stream_id stays used.
		s.Close(false)
		return nil, err
	}
	return s, nil
}

// Send sends f on the stream. Credit-consuming frames wait for the agent's
// credit; other frames go out at once.
func (s *Stream) Send(ctx context.Context, f *ownsandv1.DataFrame) error {
	f.StreamId, f.SandboxId = s.ID, s.SandboxID
	if datapath.ConsumesCredit(f.GetKind()) {
		ctx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-s.conn.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		if err := s.send.Consume(ctx, datapath.FrameCost(f)); err != nil {
			if s.conn.ctx.Err() != nil {
				return ErrConnClosed
			}
			return err
		}
	}
	return s.conn.enqueue(ctx, f)
}

// push queues a frame from the agent. Frames that consume credit are bounded
// by the receive window. Frames that carry no credit bypass it, so push
// bounds them by count and by bytes and returns ErrControlFlood beyond a
// limit; dispatch then closes the connection, like a window violation. A
// compliant agent sends one request accepted frame and one terminal frame
// per stream, which never reaches the limits even when the handler is slow.
func (s *Stream) push(f *ownsandv1.DataFrame) error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	if !datapath.ConsumesCredit(f.GetKind()) {
		size := int64(datapath.EncodedSize(f))
		if s.controlFrames+1 > s.conn.limits.MaxQueuedControlFrames || s.controlBytes+size > s.conn.limits.MaxQueuedControlBytes {
			s.mu.Unlock()
			return ErrControlFlood
		}
		s.controlFrames++
		s.controlBytes += size
	}
	s.queue = append(s.queue, f)
	ch := s.notify
	s.notify = make(chan struct{})
	s.mu.Unlock()
	close(ch)
	return nil
}

func (s *Stream) fail(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	ch := s.notify
	s.notify = make(chan struct{})
	s.mu.Unlock()
	close(ch)
}

// Next returns the next frame from the agent. It returns the stream error
// when the stream or connection failed, and ctx.Err() when ctx ended. wake,
// when not nil, makes Next return (nil, nil) when it fires, for timers.
func (s *Stream) Next(ctx context.Context, wake <-chan time.Time) (*ownsandv1.DataFrame, error) {
	for {
		s.mu.Lock()
		if len(s.queue) > 0 {
			f := s.queue[0]
			s.queue[0] = nil
			s.queue = s.queue[1:]
			if !datapath.ConsumesCredit(f.GetKind()) {
				s.controlFrames--
				s.controlBytes -= int64(datapath.EncodedSize(f))
			}
			s.mu.Unlock()
			return f, nil
		}
		if s.err != nil {
			err := s.err
			s.mu.Unlock()
			return nil, err
		}
		ch := s.notify
		s.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
			return nil, nil
		}
	}
}

// Consumed returns the credit of a frame that the caller consumed to the
// agent with a window update.
func (s *Stream) Consumed(ctx context.Context, f *ownsandv1.DataFrame) {
	if !datapath.ConsumesCredit(f.GetKind()) {
		return
	}
	inc, err := s.recv.Release(datapath.FrameCost(f))
	if err != nil || inc == 0 {
		return
	}
	_ = s.conn.enqueue(ctx, datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: inc}))
}

// Close removes the stream from its connection. sendClose sends a relay
// stream close frame, which the control plane does whenever an RPC ends
// for a reason other than an agent session-end or error frame.
func (s *Stream) Close(sendClose bool) {
	var final *ownsandv1.DataFrame
	if sendClose {
		final = datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RelayStreamClose{})
	}
	s.closeWith(final)
}

// closeWith removes the stream from its connection like Close and enqueues
// final, when not nil, as the last frame of the stream. The enqueue is best
// effort and never blocks, because the callers include the receive loop: a
// closed connection ends the stream on the agent too. A stream that already
// closed sends nothing.
func (s *Stream) closeWith(final *ownsandv1.DataFrame) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.queue = nil
	s.controlFrames, s.controlBytes = 0, 0
	s.mu.Unlock()
	c := s.conn
	c.mu.Lock()
	if c.streams[s.ID] == s {
		delete(c.streams, s.ID)
	}
	if c.perGrant[s.GrantID]--; c.perGrant[s.GrantID] <= 0 {
		delete(c.perGrant, s.GrantID)
	}
	c.mu.Unlock()
	if final != nil {
		select {
		case c.out <- final:
		default:
		}
	}
}

// Registry maps devices to their current relay connection.
type Registry struct {
	mu      sync.Mutex
	conns   map[string]*Conn
	changed chan struct{}
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{conns: map[string]*Conn{}, changed: make(chan struct{})}
}

// Register makes c the device's connection and closes an older one, whose
// RPCs end with unavailable.
func (r *Registry) Register(c *Conn) {
	r.mu.Lock()
	old := r.conns[c.DeviceID]
	r.conns[c.DeviceID] = c
	ch := r.changed
	r.changed = make(chan struct{})
	r.mu.Unlock()
	close(ch)
	if old != nil {
		old.Close(ErrReplaced)
	}
}

// Unregister removes c when it is still current.
func (r *Registry) Unregister(c *Conn) {
	r.mu.Lock()
	if r.conns[c.DeviceID] == c {
		delete(r.conns, c.DeviceID)
	}
	r.mu.Unlock()
}

// Get returns the device's open connection bound to gen, or nil.
func (r *Registry) Get(deviceID string, gen uint64) *Conn {
	r.mu.Lock()
	defer r.mu.Unlock()
	c := r.conns[deviceID]
	if c == nil || c.Generation != gen || c.ctx.Err() != nil {
		return nil
	}
	return c
}

// Wait returns the device's connection, waiting up to timeout for one to
// register.
func (r *Registry) Wait(ctx context.Context, deviceID string, gen uint64, timeout time.Duration) *Conn {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		r.mu.Lock()
		ch := r.changed
		r.mu.Unlock()
		if c := r.Get(deviceID, gen); c != nil {
			return c
		}
		select {
		case <-ch:
		case <-deadline.C:
			return nil
		case <-ctx.Done():
			return nil
		}
	}
}

// CloseChannel closes the device's connections bound to an agent channel
// generation that closed.
func (r *Registry) CloseChannel(deviceID string, gen uint64) {
	r.mu.Lock()
	c := r.conns[deviceID]
	r.mu.Unlock()
	if c != nil && c.Generation == gen {
		c.Close(errors.New("the agent channel closed"))
	}
}
