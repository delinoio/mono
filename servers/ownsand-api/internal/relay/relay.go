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
}

// DefaultLimits returns the recorded limits.
func DefaultLimits() Limits {
	return Limits{
		StreamBuffer:       4 << 20,
		MaxStreamsPerConn:  64,
		MaxStreamsPerGrant: 4,
		Keepalive:          30 * time.Second,
		IdleTimeout:        90 * time.Second,
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

	lastRecv atomic.Int64
	lastSend atomic.Int64

	mu        sync.Mutex
	next      uint64
	streams   map[uint64]*Stream
	perGrant  map[string]int
	peerKinds map[ownsandv1.DataFrameKind]bool
}

// NewConn returns a connection for the device bound to an agent channel
// generation.
func NewConn(parent context.Context, deviceID string, gen uint64, limits Limits) *Conn {
	ctx, cancel := context.WithCancelCause(parent)
	c := &Conn{DeviceID: deviceID, Generation: gen, limits: limits, out: make(chan *ownsandv1.DataFrame, outboundQueue),
		ctx: ctx, cancel: cancel, streams: map[uint64]*Stream{}, perGrant: map[string]int{}}
	now := time.Now().UnixNano()
	c.lastRecv.Store(now)
	c.lastSend.Store(now)
	return c
}

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
	grace := time.NewTimer(writerGrace)
	defer grace.Stop()
	select {
	case <-writerDone:
	case <-grace.C:
	}
	return c.Err()
}

func (c *Conn) dispatch(f *ownsandv1.DataFrame) error {
	if datapath.CheckFrameSize(f) != nil {
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
	if err := datapath.ValidateFrame(f); err != nil {
		var unknown *datapath.UnknownKindError
		if errors.As(err, &unknown) {
			s.fail(ErrUnknownKind)
			s.Close(true)
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
	s.push(f)
	return nil
}

// Stream is one relay stream.
type Stream struct {
	ID        uint64
	GrantID   string
	SandboxID string
	conn      *Conn
	recv      *datapath.ReceiveWindow
	send      *datapath.SendWindow

	mu     sync.Mutex
	queue  []*ownsandv1.DataFrame
	notify chan struct{}
	err    error
	closed bool
	// Started is when the stream_id was allocated.
	Started time.Time
}

// Open allocates a new stream_id (increasing from 1, never reused) and
// sends the stream open frame with the first credit window.
func (c *Conn) Open(ctx context.Context, grantID, sandboxID string) (*Stream, error) {
	recv, err := datapath.NewReceiveWindow(c.limits.StreamBuffer)
	if err != nil {
		return nil, err
	}
	send, _ := datapath.NewSendWindow(0)
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

func (s *Stream) push(f *ownsandv1.DataFrame) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.queue = append(s.queue, f)
	ch := s.notify
	s.notify = make(chan struct{})
	s.mu.Unlock()
	close(ch)
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
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.queue = nil
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
	if sendClose {
		// Best effort: a closed connection ends the stream on the agent too.
		select {
		case c.out <- datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RelayStreamClose{}):
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
