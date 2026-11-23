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
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/proto"

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

// outboundQueue bounds the ordinary frames waiting for the connection writer.
// Every admitted stream also owns one reserved place for its final frame (see
// Conn.out), so the physical queue holds outboundQueue plus
// Limits.MaxStreamsPerConn frames at most.
const outboundQueue = 1024

// Errors.
var (
	ErrConnClosed     = errors.New("the agent relay connection closed")
	ErrReplaced       = errors.New("a newer agent relay connection replaced this one")
	ErrStaleChannel   = errors.New("the agent channel of this relay connection is no longer current")
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

	// out is the outbound queue drained by the single writer. Its capacity is
	// outboundQueue plus MaxStreamsPerConn. An ordinary frame takes one of the
	// outboundQueue tokens of slots first and returns the token when the writer
	// dequeues it, so ordinary frames never use more than outboundQueue places.
	// A stream's final frame (stream close or error) bypasses the tokens and
	// uses the place that the stream reserved by being admitted: Open admits a
	// stream only while the open streams and the finals that wait for the
	// writer stay within MaxStreamsPerConn, so the physical queue always has
	// room for every final and closeWith never blocks or drops one.
	out    chan outFrame
	slots  chan struct{}
	ctx    context.Context
	cancel context.CancelCauseFunc

	// sendInterrupt is set by SetSendInterrupt before Run starts.
	sendInterrupt func() error
	// wireBytes is set by SetWireBytes before Run starts.
	wireBytes func() int64

	// clk supplies the monotonic readings of the idle and keepalive timers and
	// the wall timestamps of the observers (see connClock).
	clk connClock
	// lastRecv and lastSend are the monotonic readings (clk.mono, in
	// nanoseconds) of the last received frame and the last sent frame. The
	// idle and keepalive thresholds use them, so wall-clock steps do not
	// change them.
	lastRecv atomic.Int64
	lastSend atomic.Int64
	// lastRecvWall is the wall timestamp (Unix nanoseconds) that belongs to
	// lastRecv, for LastReceive.
	lastRecvWall atomic.Int64
	// lastLarge is the wall timestamp (Unix nanoseconds) of the last large
	// frame that the connection received on an open stream (zero before the
	// first one); see LargeFrameBytes.
	lastLarge atomic.Int64

	// openGate serializes Open: a holder allocates a stream_id and enqueues
	// the stream open frame before the next Open starts, so the stream open
	// frames reach the agent in increasing stream_id order. It is a channel
	// and not a mutex so that a waiting Open honors its context and the
	// connection closing.
	openGate chan struct{}

	mu      sync.Mutex
	next    uint64
	streams map[uint64]*Stream
	// pendingFinals counts the final frames that wait for the writer (in out
	// or in its hands). They keep their admission slot, in pendingFinals for
	// the connection and in perGrant for the grant, until the writer is done
	// with them.
	pendingFinals int
	// perGrant counts, for each grant, the open streams plus the pending
	// final frames.
	perGrant  map[string]int
	peerKinds map[ownsandv1.DataFrameKind]bool

	// writeWaiters maps a frame that SendConfirmed queued to the channel on
	// which the writer reports the result of its send (see writeConfirmed).
	// The queue and the outFrame stay unchanged for every other sender.
	writeWaiters sync.Map // *ownsandv1.DataFrame -> chan error
}

// outFrame is a frame in the outbound queue. final marks a stream's final
// frame, which owns a reserved place instead of an ordinary token.
type outFrame struct {
	frame   *ownsandv1.DataFrame
	final   bool
	grantID string
	// onWritten, when set, receives the bytes that the send of the frame
	// wrote (see Stream.SendMetered).
	onWritten func(n int64)
}

// NewConn returns a connection for the device bound to an agent channel
// generation.
func NewConn(parent context.Context, deviceID string, gen uint64, limits Limits) *Conn {
	return newConnWithClock(parent, deviceID, gen, limits, newSystemClock())
}

// newConnWithClock is NewConn with an injected clock, so tests can step the
// wall clock and advance the monotonic clock independently.
func newConnWithClock(parent context.Context, deviceID string, gen uint64, limits Limits, clk connClock) *Conn {
	if limits.MaxQueuedControlFrames <= 0 {
		limits.MaxQueuedControlFrames = defaultMaxQueuedControlFrames
	}
	if limits.MaxQueuedControlBytes <= 0 {
		limits.MaxQueuedControlBytes = defaultMaxQueuedControlBytes
	}
	ctx, cancel := context.WithCancelCause(parent)
	reserved := max(limits.MaxStreamsPerConn, 0)
	c := &Conn{DeviceID: deviceID, Generation: gen, limits: limits, out: make(chan outFrame, outboundQueue+reserved),
		slots: make(chan struct{}, outboundQueue), ctx: ctx, cancel: cancel, openGate: make(chan struct{}, 1), streams: map[uint64]*Stream{}, perGrant: map[string]int{}, clk: clk}
	c.lastRecv.Store(int64(clk.mono()))
	c.lastSend.Store(int64(clk.mono()))
	c.lastRecvWall.Store(clk.wall().UnixNano())
	return c
}

// SetSendInterrupt registers the function that makes a blocked send fail
// (for an HTTP response, a write deadline in the past). Run calls it once
// after the connection closed. It returns an error when it cannot interrupt
// the send; Run then falls back to a bounded wait for the writer. Call it
// before Run.
func (c *Conn) SetSendInterrupt(interrupt func() error) { c.sendInterrupt = interrupt }

// SetWireBytes registers a function that returns the total bytes written to
// the underlying connection so far. The writer uses it to measure the bytes
// of a metered frame on the wire (see Stream.SendMetered); without it, a
// metered frame counts its encoded ConnectResponse message plus the 5-byte
// envelope prefix. Call it before Run.
func (c *Conn) SetWireBytes(written func() int64) { c.wireBytes = written }

// envelopePrefix is the size of the Connect and gRPC message envelope
// prefix (one flag byte and a 4-byte length).
const envelopePrefix = 5

// sendMeasured sends of.frame with send, reports its bytes to of.onWritten
// (see sendCounted) and tells a SendConfirmed caller that waits for the frame
// that the send returned.
func (c *Conn) sendMeasured(of outFrame, send func(*ownsandv1.DataFrame) error) error {
	err := c.sendCounted(of, send)
	c.writeConfirmed(of.frame, err)
	return err
}

// sendCounted sends of.frame with send and reports its bytes to
// of.onWritten: the bytes that the send wrote, also when it failed after a
// partial write.
func (c *Conn) sendCounted(of outFrame, send func(*ownsandv1.DataFrame) error) error {
	if of.onWritten == nil {
		return send(of.frame)
	}
	if c.wireBytes == nil {
		err := send(of.frame)
		if err == nil {
			of.onWritten(int64(proto.Size(&ownsandv1.ConnectResponse{Frame: of.frame})) + envelopePrefix)
		}
		return err
	}
	before := c.wireBytes()
	err := send(of.frame)
	if n := c.wireBytes() - before; n > 0 {
		of.onWritten(n)
	}
	return err
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
// It returns a wall timestamp; the idle timeout does not use it (see lastRecv).
func (c *Conn) LastReceive() time.Time { return time.Unix(0, c.lastRecvWall.Load()) }

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

// enqueue queues an ordinary frame. It waits for one of the outboundQueue
// tokens, and returns when ctx ends or the connection closes.
func (c *Conn) enqueue(ctx context.Context, f *ownsandv1.DataFrame) error {
	return c.enqueueFrame(ctx, outFrame{frame: f})
}

// enqueueFrame queues an ordinary outFrame; see enqueue.
func (c *Conn) enqueueFrame(ctx context.Context, of outFrame) error {
	select {
	case c.slots <- struct{}{}:
	case <-c.ctx.Done():
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	// The token guarantees room: ordinary frames never exceed outboundQueue,
	// and the queue also holds the reserved places of the finals.
	c.out <- of
	return nil
}

// dequeued runs when the writer took of from the queue. An ordinary frame
// returns its token at once, so enqueue can proceed while the frame is sent.
func (c *Conn) dequeued(of outFrame) {
	if !of.final {
		<-c.slots
	}
}

// sent runs when the writer is done with of (sent or failed). A final frame
// then releases the admission slots of its stream.
func (c *Conn) sent(of outFrame) {
	if !of.final {
		return
	}
	c.mu.Lock()
	c.releaseFinalLocked(of.grantID)
	c.mu.Unlock()
}

// releaseFinalLocked releases the admission slots that a final frame kept.
func (c *Conn) releaseFinalLocked(grantID string) {
	c.pendingFinals--
	if c.perGrant[grantID]--; c.perGrant[grantID] <= 0 {
		delete(c.perGrant, grantID)
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
				if c.clk.mono()-time.Duration(c.lastRecv.Load()) > c.limits.IdleTimeout {
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
			case of := <-c.out:
				c.dequeued(of)
				err := c.sendMeasured(of, guardedSend)
				c.sent(of)
				if err != nil {
					c.Close(err)
					return
				}
				c.lastSend.Store(int64(c.clk.mono()))
			case <-ticker.C:
				now := c.clk.mono()
				if now-time.Duration(c.lastSend.Load()) >= c.limits.Keepalive {
					if err := guardedSend(datapath.MustNewFrame(0, "", &ownsandv1.RelayKeepalive{})); err != nil {
						c.Close(err)
						return
					}
					c.lastSend.Store(int64(now))
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
			c.lastRecv.Store(int64(c.clk.mono()))
			c.lastRecvWall.Store(c.clk.wall().UnixNano())
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
		c.lastLarge.Store(c.clk.wall().UnixNano())
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
	if len(c.streams)+c.pendingFinals >= c.limits.MaxStreamsPerConn || c.perGrant[grantID] >= c.limits.MaxStreamsPerGrant {
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
	return s.SendMetered(ctx, f, nil)
}

// SendMetered is Send with a callback that receives the bytes of f on the
// agent connection after the writer wrote it (relay egress metering). It is
// not called when the frame is never written.
func (s *Stream) SendMetered(ctx context.Context, f *ownsandv1.DataFrame, onWritten func(n int64)) error {
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
	return s.enqueueFrame(ctx, outFrame{frame: f, onWritten: onWritten})
}

// enqueueFrame queues an ordinary frame of this stream. It waits for one of
// the connection's outboundQueue tokens like Conn.enqueueFrame, and then
// inserts the frame only while the stream is open: the check and the insert
// happen under s.mu, which closeWith also holds when it sets closed, so a
// frame can never enter the queue behind the stream's final frame. A frame of
// a closed stream returns its token, is not queued (its onWritten callback
// never runs) and fails with ErrStreamClosed. The insert never blocks, because
// the token guarantees room; the wait for the token happens outside s.mu.
func (s *Stream) enqueueFrame(ctx context.Context, of outFrame) error {
	c := s.conn
	if s.isClosed() {
		return ErrStreamClosed
	}
	select {
	case c.slots <- struct{}{}:
	case <-c.ctx.Done():
		return ErrConnClosed
	case <-ctx.Done():
		return ctx.Err()
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		<-c.slots
		return ErrStreamClosed
	}
	// The token guarantees room, see Conn.enqueueFrame.
	c.out <- of
	s.mu.Unlock()
	return nil
}

func (s *Stream) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
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
	// A closed stream returns ErrStreamClosed: the agent ended the stream on
	// its side already, so the window update has no receiver.
	_ = s.enqueueFrame(ctx, outFrame{frame: datapath.MustNewFrame(s.ID, s.SandboxID, &ownsandv1.RelayWindowUpdate{IncrementBytes: inc})})
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
// final, when not nil, as the last frame of the stream. The enqueue never
// blocks, because the callers include the receive loop, and never drops the
// frame: the stream reserved a place for it when Open admitted it. The stream
// keeps its connection and grant admission slots until the writer is done with
// final, so the pending finals stay bounded by MaxStreamsPerConn. A closed
// connection ends the stream on the agent too, so then no frame is queued. A
// stream that already closed sends nothing.
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
	if final != nil && c.ctx.Err() == nil {
		// Keep the admission slots, and with them the reserved place.
		c.pendingFinals++
		select {
		case c.out <- outFrame{frame: final, final: true, grantID: s.GrantID}:
			c.mu.Unlock()
			return
		default:
			// Unreachable while the admission invariant holds: log it and
			// release the slots instead of leaking them.
			c.releaseFinalLocked(s.GrantID)
			c.mu.Unlock()
			slog.Error("relay final frame dropped: the outbound queue has no reserved place",
				slog.String("device_id", c.DeviceID), slog.Uint64("stream_id", s.ID), slog.String("grant_id", s.GrantID))
			return
		}
	}
	if c.perGrant[s.GrantID]--; c.perGrant[s.GrantID] <= 0 {
		delete(c.perGrant, s.GrantID)
	}
	c.mu.Unlock()
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
// RPCs end with unavailable. current reports whether the agent channel that c
// is bound to is still the device's current channel; it runs under the
// registry lock, so a stale channel cannot replace a newer connection between
// the caller's channel lookup and the swap. When it returns false, Register
// changes nothing and returns ErrStaleChannel. current must not call back
// into the registry. A nil current accepts every connection.
func (r *Registry) Register(c *Conn, current func() bool) error {
	r.mu.Lock()
	if current != nil && !current() {
		r.mu.Unlock()
		return ErrStaleChannel
	}
	old := r.conns[c.DeviceID]
	r.conns[c.DeviceID] = c
	ch := r.changed
	r.changed = make(chan struct{})
	r.mu.Unlock()
	close(ch)
	if old != nil {
		old.Close(ErrReplaced)
	}
	return nil
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
