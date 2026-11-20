package datapath

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/controlplane"
	sdkdp "github.com/delinoio/mono/packages/ownsand-sdk-go/datapath"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

var (
	errGrantInvalid     = errors.New("no live relay grant")
	errNoStreamCapacity = errors.New("the agent is at its relay stream capacity")
)

// RelayLimits are the agent's relay caps and timings.
type RelayLimits struct {
	// StreamWindow is the receive credit per stream, at least one maximum
	// frame.
	StreamWindow int64
	// MaxStreams caps the open streams per relay connection.
	MaxStreams int
	// MaxStreamsPerGrant caps the open streams of one grant.
	MaxStreamsPerGrant int
	// MaxCredit caps the sum of the receive credit that the agent grants on
	// one connection.
	MaxCredit int64
	// ReceiveTimeout: no received byte for this long is a lost connection.
	ReceiveTimeout time.Duration
	// KeepaliveAfter: a keepalive after this long without an outbound frame.
	KeepaliveAfter time.Duration
	// ProbeAfter and ProbeWait: the echo probe before an early reconnect.
	ProbeAfter time.Duration
	ProbeWait  time.Duration
	// IdleClose closes a connection without a live grant and open stream.
	IdleClose time.Duration
	// MaxBackoff bounds the reconnect back-off.
	MaxBackoff time.Duration
}

// DefaultRelayLimits are the recorded production values.
func DefaultRelayLimits() RelayLimits {
	return RelayLimits{
		StreamWindow:       2 << 20,
		MaxStreams:         256,
		MaxStreamsPerGrant: 8,
		MaxCredit:          64 << 20,
		ReceiveTimeout:     90 * time.Second,
		KeepaliveAfter:     30 * time.Second,
		ProbeAfter:         35 * time.Second,
		ProbeWait:          5 * time.Second,
		IdleClose:          60 * time.Second,
		MaxBackoff:         5 * time.Second,
	}
}

type connKey struct{}

// byteCounter wraps the response body of a relay connection so that every
// received byte, also of a large frame still in transit, counts as activity.
type byteCounter struct {
	base http.RoundTripper
}

func (b byteCounter) RoundTrip(req *http.Request) (*http.Response, error) {
	res, err := b.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if rc, ok := req.Context().Value(connKey{}).(*relayConn); ok && res.Body != nil {
		res.Body = &countingBody{ReadCloser: res.Body, c: rc}
	}
	return res, nil
}

type countingBody struct {
	io.ReadCloser
	c *relayConn
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.c.touch()
	}
	return n, err
}

// relayManager keeps one RelayAgentService.Connect connection while a live
// grant exists.
type relayManager struct {
	log    *slog.Logger
	lim    RelayLimits
	disp   *Dispatcher
	grants *grantTable
	client ownsandv1connect.RelayAgentServiceClient
	auth   func(ctx context.Context) (string, error)

	mu   sync.Mutex
	conn *relayConn
	wake chan struct{}
	// dials counts connection attempts (tests).
	dials atomic.Int64
}

func newRelayManager(log *slog.Logger, lim RelayLimits, disp *Dispatcher, grants *grantTable, origin string, auth func(context.Context) (string, error)) (*relayManager, error) {
	hc, err := controlplane.NewHTTPClient(origin)
	if err != nil {
		return nil, err
	}
	hc.Transport = byteCounter{base: hc.Transport}
	return &relayManager{
		log: log, lim: lim, disp: disp, grants: grants, auth: auth,
		client: ownsandv1connect.NewRelayAgentServiceClient(hc, origin),
		wake:   make(chan struct{}, 1),
	}, nil
}

func (m *relayManager) notify() {
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// run dials while a live grant exists and serves each connection.
func (m *relayManager) run(ctx context.Context) {
	backoff := 500 * time.Millisecond
	for ctx.Err() == nil {
		if !m.grants.hasLive() {
			select {
			case <-ctx.Done():
				return
			case <-m.wake:
			case <-time.After(time.Second):
			}
			continue
		}
		m.dials.Add(1)
		c, err := m.dial(ctx)
		if err != nil {
			m.log.Warn("relay connection failed", "error", err, "retry_in", backoff.String())
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, m.lim.MaxBackoff)
			continue
		}
		backoff = 500 * time.Millisecond
		reason := c.serve()
		m.mu.Lock()
		if m.conn == c {
			m.conn = nil
		}
		m.mu.Unlock()
		m.log.Info("relay connection closed", "reason", reason.String())
		if reason == closeProbeFailed {
			continue // dial again at once
		}
		if reason != closeIdle && reason != closeChannelLost {
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			backoff = min(2*backoff, m.lim.MaxBackoff)
		}
	}
}

// closeChannel closes the connection when the agent channel closes. A new
// connection is dialed only after a new channel delivered a new grant.
func (m *relayManager) closeChannel() {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c != nil {
		c.close(closeChannelLost)
	}
}

// probe sends an echo request when a new grant arrives and the connection
// has received nothing for ProbeAfter.
func (m *relayManager) probe() {
	m.mu.Lock()
	c := m.conn
	m.mu.Unlock()
	if c != nil {
		c.maybeProbe()
	}
	m.notify()
}

type closeReason int

const (
	closeLost closeReason = iota + 1
	closeIdle
	closeChannelLost
	closeProbeFailed
	closeProtocol
	closeStopped
)

func (r closeReason) String() string {
	return [...]string{"", "lost", "idle", "agent_channel_lost", "probe_failed", "protocol_error", "stopped"}[r]
}

// relayConn is one RelayAgentService.Connect stream.
type relayConn struct {
	m      *relayManager
	stream *connect.BidiStreamForClient[ownsandv1.DataFrame, ownsandv1.DataFrame]
	ctx    context.Context
	cancel context.CancelFunc
	start  time.Time

	lastByte atomic.Int64 // nanoseconds since start
	lastSend atomic.Int64
	probeAt  atomic.Int64 // 0: no probe

	wmu sync.Mutex

	mu        sync.Mutex
	streams   map[uint64]*relayStream
	maxSeen   uint64
	credit    int64
	peerKinds []ownsandv1.DataFrameKind
	reason    closeReason
	idleSince time.Time
	closed    bool
}

func (c *relayConn) since() int64 { return int64(time.Since(c.start)) }
func (c *relayConn) touch()       { c.lastByte.Store(c.since()) }

func (m *relayManager) dial(parent context.Context) (*relayConn, error) {
	// Each relay connection gets a new device session credential.
	cred, err := m.auth(parent)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	c := &relayConn{m: m, ctx: ctx, cancel: cancel, start: time.Now(), streams: map[uint64]*relayStream{}, idleSince: time.Now()}
	ctx = context.WithValue(ctx, connKey{}, c)
	c.stream = m.client.Connect(ctx)
	c.stream.RequestHeader().Set("Authorization", controlplane.BearerHeader(cred))
	// The request headers go out with an initial keepalive frame at once,
	// then the supported frame kinds.
	if err := c.send(keepaliveFrame(false)); err != nil {
		cancel()
		return nil, err
	}
	if err := c.send(sdkdp.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_SupportedKinds{SupportedKinds: &ownsandv1.SupportedFrameKinds{Kinds: sdkdp.SupportedKinds()}}
	})); err != nil {
		cancel()
		return nil, err
	}
	c.touch()
	m.mu.Lock()
	m.conn = c
	m.mu.Unlock()
	m.log.Info("relay connection opened")
	return c, nil
}

func keepaliveFrame(echo bool) *ownsandv1.DataFrame {
	return sdkdp.NewFrame(0, "", func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_Keepalive{Keepalive: &ownsandv1.FrameKeepalive{EchoRequested: echo}}
	})
}

func (c *relayConn) send(f *ownsandv1.DataFrame) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if err := c.stream.Send(f); err != nil {
		return err
	}
	c.lastSend.Store(c.since())
	return nil
}

// closeGrace bounds a graceful close: the agent half-closes its side so
// frames already written (for example, session-end frames) reach the
// control plane, and cancels the stream after this time at the latest.
const closeGrace = 2 * time.Second

func (c *relayConn) close(r closeReason) {
	c.mu.Lock()
	first := c.reason == 0
	if first {
		c.reason = r
	}
	c.mu.Unlock()
	if !first {
		return
	}
	if r == closeChannelLost || r == closeIdle || r == closeStopped {
		c.wmu.Lock()
		_ = c.stream.CloseRequest()
		c.wmu.Unlock()
		time.AfterFunc(closeGrace, c.cancel)
		return
	}
	c.cancel()
}

func (c *relayConn) maybeProbe() {
	if time.Duration(c.since()-c.lastByte.Load()) < c.m.lim.ProbeAfter || c.probeAt.Load() != 0 {
		return
	}
	c.probeAt.Store(c.since())
	_ = c.send(keepaliveFrame(true))
	c.m.log.Info("relay connection idle; echo probe sent")
}

// serve runs the receive loop and the watchdog until the connection ends,
// then ends every stream of the connection.
func (c *relayConn) serve() closeReason {
	recvErr := make(chan error, 1)
	go func() {
		for {
			f, err := c.stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			c.touch()
			c.probeAt.Store(0)
			if !c.handle(f) {
				c.close(closeProtocol)
			}
		}
	}()
	t := time.NewTicker(200 * time.Millisecond)
	defer t.Stop()
loop:
	for {
		select {
		case <-recvErr:
			c.close(closeLost)
			c.cancel()
			break loop
		case <-c.ctx.Done():
			break loop
		case <-t.C:
			c.watchdog()
		}
	}
	c.cancel()
	c.wmu.Lock()
	_ = c.stream.CloseRequest()
	c.wmu.Unlock()
	_ = c.stream.CloseResponse()
	c.mu.Lock()
	c.closed = true
	streams := c.streams
	c.streams = map[uint64]*relayStream{}
	reason := c.reason
	c.mu.Unlock()
	// A lost or replaced connection ends its streams like a relay stream
	// close: output subscriptions are released, operations continue.
	for _, s := range streams {
		s.closeQuietly()
	}
	if reason == 0 {
		reason = closeStopped
	}
	return reason
}

func (c *relayConn) watchdog() {
	now := c.since()
	lim := c.m.lim
	if time.Duration(now-c.lastByte.Load()) >= lim.ReceiveTimeout {
		c.m.log.Warn("relay connection received nothing for the receive timeout")
		c.close(closeLost)
		return
	}
	if p := c.probeAt.Load(); p != 0 && time.Duration(now-p) >= lim.ProbeWait && c.lastByte.Load() < p {
		c.m.log.Warn("relay echo probe unanswered; reconnecting")
		c.close(closeProbeFailed)
		return
	}
	if time.Duration(now-c.lastSend.Load()) >= lim.KeepaliveAfter {
		_ = c.send(keepaliveFrame(false))
	}
	c.mu.Lock()
	open := len(c.streams)
	if open > 0 || c.m.grants.hasLive() {
		c.idleSince = time.Now()
	}
	idle := time.Since(c.idleSince)
	c.mu.Unlock()
	if idle >= lim.IdleClose {
		c.close(closeIdle)
	}
}

// handle processes one received frame. It returns false for a protocol
// violation that closes the connection.
func (c *relayConn) handle(f *ownsandv1.DataFrame) bool {
	if f.GetStreamId() == 0 {
		switch p := f.GetPayload().(type) {
		case *ownsandv1.DataFrame_Keepalive:
			if p.Keepalive.GetEchoRequested() {
				_ = c.send(keepaliveFrame(false))
			}
		case *ownsandv1.DataFrame_SupportedKinds:
			c.mu.Lock()
			c.peerKinds = p.SupportedKinds.GetKinds()
			c.mu.Unlock()
		default:
			// Unknown connection-level frames are ignored, so either side
			// can be updated first.
		}
		return true
	}
	id := f.GetStreamId()
	if open := f.GetRelayStreamOpen(); open != nil {
		c.openStream(id, f.GetSandboxId(), open)
		return true
	}
	c.mu.Lock()
	s := c.streams[id]
	c.mu.Unlock()
	if s == nil {
		return true // a frame of a closed or unknown stream is dropped
	}
	switch p := f.GetPayload().(type) {
	case *ownsandv1.DataFrame_RelayWindowUpdate:
		s.addSend(p.RelayWindowUpdate.GetAddedBytes())
	case *ownsandv1.DataFrame_RelayStreamClose:
		s.closeQuietly()
	default:
		if sdkdp.KindOf(f) == ownsandv1.DataFrameKind_DATA_FRAME_KIND_UNSPECIFIED {
			s.fail(connect.CodeUnimplemented, "unknown frame kind")
			return true
		}
		if !IsRequest(f) {
			s.fail(connect.CodeInvalidArgument, "unexpected frame on a relay stream")
			return true
		}
		size := sdkdp.FrameSize(f)
		if !s.takeRecv(size) {
			// The control plane sent beyond the window that the agent
			// granted: a protocol violation.
			s.fail(connect.CodeResourceExhausted, "request frame exceeds the granted window")
			return true
		}
		s.request(f, size)
	}
	return true
}

func (c *relayConn) openStream(id uint64, sandboxID string, open *ownsandv1.RelayStreamOpen) {
	c.mu.Lock()
	if id <= c.maxSeen || c.closed {
		c.mu.Unlock()
		return // stream IDs are never reused on one connection
	}
	c.maxSeen = id
	s := &relayStream{c: c, id: id, sandboxID: sandboxID, sendWin: open.GetInitialWindow(), changed: make(chan struct{})}
	s.ctx, s.cancel = context.WithCancel(c.ctx)
	reject := func(code connect.Code, msg string) {
		c.mu.Unlock()
		_ = c.send(errorFrame(id, sandboxID, code, msg))
	}
	// Caps apply before the agent allocates stream state.
	if len(c.streams) >= c.m.lim.MaxStreams || c.credit+c.m.lim.StreamWindow > c.m.lim.MaxCredit {
		reject(connect.CodeResourceExhausted, errNoStreamCapacity.Error())
		return
	}
	g, err := c.m.grants.openStream(open.GetGrantId(), sandboxID, s, c.m.lim.MaxStreamsPerGrant)
	if err != nil {
		if errors.Is(err, errGrantInvalid) {
			c.mu.Unlock()
			_ = c.send(sessionEndFrame(id, sandboxID, ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID))
			return
		}
		reject(connect.CodeOf(err), errNoStreamCapacity.Error())
		return
	}
	s.g = g
	s.recvWin = c.m.lim.StreamWindow
	c.credit += c.m.lim.StreamWindow
	c.streams[id] = s
	c.mu.Unlock()
	// The first window for request payload frames.
	_ = c.send(windowFrame(id, sandboxID, c.m.lim.StreamWindow))
}

func errorFrame(id uint64, sandboxID string, code connect.Code, msg string) *ownsandv1.DataFrame {
	return sdkdp.NewFrame(id, sandboxID, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_Error{Error: &ownsandv1.FrameError{Code: code.String(), Message: msg}}
	})
}

func sessionEndFrame(id uint64, sandboxID string, r ownsandv1.SessionEndReason) *ownsandv1.DataFrame {
	return sdkdp.NewFrame(id, sandboxID, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_SessionEnd{SessionEnd: &ownsandv1.SessionEnd{Reason: r}}
	})
}

func windowFrame(id uint64, sandboxID string, n int64) *ownsandv1.DataFrame {
	return sdkdp.NewFrame(id, sandboxID, func(f *ownsandv1.DataFrame) {
		f.Payload = &ownsandv1.DataFrame_RelayWindowUpdate{RelayWindowUpdate: &ownsandv1.RelayWindowUpdate{AddedBytes: n}}
	})
}

// relayStream is one relay stream; it is the Sink of its request.
type relayStream struct {
	c         *relayConn
	id        uint64
	sandboxID string
	g         *grant
	ctx       context.Context
	cancel    context.CancelFunc

	mu        sync.Mutex
	sendWin   int64
	recvWin   int64
	requested bool
	ended     bool
	changed   chan struct{}
}

func (s *relayStream) Context() context.Context { return s.ctx }

func (s *relayStream) addSend(n int64) {
	if n <= 0 {
		return
	}
	s.mu.Lock()
	s.sendWin += n
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

func (s *relayStream) takeRecv(n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > s.recvWin {
		return false
	}
	s.recvWin -= n
	return true
}

// Payload sends a payload frame within the send window that the control
// plane granted for this stream.
func (s *relayStream) Payload(set func(*ownsandv1.DataFrame)) error {
	f := sdkdp.NewFrame(s.id, s.sandboxID, set)
	size := sdkdp.FrameSize(f)
	for {
		s.mu.Lock()
		if s.ended {
			s.mu.Unlock()
			return context.Canceled
		}
		if s.sendWin >= size {
			s.sendWin -= size
			s.mu.Unlock()
			return s.c.send(f)
		}
		ch := s.changed
		s.mu.Unlock()
		select {
		case <-ch:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}

// Error sends an error frame.
func (s *relayStream) Error(code connect.Code, msg string) error {
	return s.c.send(errorFrame(s.id, s.sandboxID, code, msg))
}

// request handles the first request frame of the stream.
func (s *relayStream) request(f *ownsandv1.DataFrame, size int64) {
	s.mu.Lock()
	if s.requested {
		s.mu.Unlock()
		s.fail(connect.CodeInvalidArgument, "a relay stream carries one request")
		return
	}
	s.requested = true
	s.mu.Unlock()
	c, g := s.c, s.g
	// The request left the buffer: return its credit.
	_ = c.send(windowFrame(s.id, s.sandboxID, size))
	if !c.m.grants.admitRequest(g) {
		s.endWith(ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID)
		return
	}
	if err := c.m.disp.CheckGrant(g.target(), f); err != nil {
		code := errCode(err)
		_ = s.Error(code, message(err))
		s.finish(Outcome{Code: code}, f)
		return
	}
	switch f.GetPayload().(type) {
	case *ownsandv1.DataFrame_ExecRequest, *ownsandv1.DataFrame_ExecAttachRequest, *ownsandv1.DataFrame_FileReadRequest:
		_ = c.send(sdkdp.NewFrame(s.id, s.sandboxID, func(f *ownsandv1.DataFrame) {
			f.Payload = &ownsandv1.DataFrame_RequestAccepted{RequestAccepted: &ownsandv1.RequestAccepted{}}
		}))
	}
	go func() {
		o := c.m.disp.Serve(g.sandboxID, f, s)
		s.finish(o, f)
	}()
}

// finish removes the stream after its final frame and applies the grant
// release rules.
func (s *relayStream) finish(o Outcome, req *ownsandv1.DataFrame) {
	if !s.remove() {
		return
	}
	attach := req != nil && req.GetExecAttachRequest() != nil && s.g.kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC
	s.c.m.grants.streamEnded(s.g, s, o, attach)
}

// remove forgets the stream; it reports false when it was already removed.
func (s *relayStream) remove() bool {
	s.mu.Lock()
	if s.ended {
		s.mu.Unlock()
		return false
	}
	s.ended = true
	s.mu.Unlock()
	s.cancel()
	c := s.c
	c.mu.Lock()
	if c.streams[s.id] == s {
		delete(c.streams, s.id)
		c.credit -= c.m.lim.StreamWindow
	}
	c.mu.Unlock()
	return true
}

// fail ends the stream with an error frame.
func (s *relayStream) fail(code connect.Code, msg string) {
	_ = s.Error(code, msg)
	s.finish(Outcome{Code: code}, nil)
}

// closeQuietly ends the stream without a frame (relay stream close or a
// lost connection). The operation continues.
func (s *relayStream) closeQuietly() {
	s.finish(Outcome{Started: true}, nil)
}

// endWith sends a session-end frame and ends the stream.
func (s *relayStream) endWith(reason ownsandv1.SessionEndReason) {
	s.mu.Lock()
	ended := s.ended
	s.mu.Unlock()
	if ended {
		return
	}
	_ = s.c.send(sessionEndFrame(s.id, s.sandboxID, reason))
	if s.remove() && reason != ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		// The grant table already dropped the grant (revocation, channel
		// loss, deadline or replacement); nothing more to release.
		return
	}
	if reason == ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_INVALID {
		s.c.m.grants.streamEnded(s.g, s, Outcome{}, false)
	}
}
