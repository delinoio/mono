// SPDX-License-Identifier: Apache-2.0

package dataplane

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"

	"github.com/delinoio/mono/cmds/ownsand-agent/internal/agent"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/cpclient"
	"github.com/delinoio/mono/cmds/ownsand-agent/internal/guestclient"
	"github.com/delinoio/mono/go/ownsand/datapath"
	"github.com/delinoio/mono/go/uuidv7"
	"github.com/delinoio/mono/internal/backoff"
	ownsandv1 "github.com/delinoio/mono/protos/delino/ownsand/v1"
	"github.com/delinoio/mono/protos/delino/ownsand/v1/ownsandv1connect"
)

// RelayLimits bound what one relay connection and one grant can hold, so a
// faulty or compromised control plane cannot exhaust device memory with
// one live grant.
type RelayLimits struct {
	// MaxGrants is the per-device grant limit (the control plane's limit).
	MaxGrants int
	// MaxStreamsPerGrant and MaxStreamsPerConnection cap open streams.
	MaxStreamsPerGrant      int
	MaxStreamsPerConnection int
	// StreamReceiveWindow is the receive buffer of each stream.
	StreamReceiveWindow int64
	// MaxConnectionCredit caps the receive credit of all streams of a
	// connection.
	MaxConnectionCredit int64
}

// DefaultRelayLimits are the recorded limits.
func DefaultRelayLimits() RelayLimits {
	return RelayLimits{
		MaxGrants:               256,
		MaxStreamsPerGrant:      8,
		MaxStreamsPerConnection: 64,
		StreamReceiveWindow:     datapath.MinWindowSize,
		MaxConnectionCredit:     64 * datapath.MinWindowSize,
	}
}

func (l RelayLimits) withDefaults() RelayLimits {
	d := DefaultRelayLimits()
	if l.MaxGrants <= 0 {
		l.MaxGrants = d.MaxGrants
	}
	if l.MaxStreamsPerGrant <= 0 {
		l.MaxStreamsPerGrant = d.MaxStreamsPerGrant
	}
	if l.MaxStreamsPerConnection <= 0 {
		l.MaxStreamsPerConnection = d.MaxStreamsPerConnection
	}
	if l.StreamReceiveWindow < datapath.MinWindowSize {
		l.StreamReceiveWindow = d.StreamReceiveWindow
	}
	if l.MaxConnectionCredit <= 0 {
		l.MaxConnectionCredit = d.MaxConnectionCredit
	}
	return l
}

type grant struct {
	id         string
	sandboxID  string
	kind       ownsandv1.RelayOperationKind
	opID       string
	digest     []byte
	ch         agent.Channel
	received   time.Time
	expiresIn  time.Duration
	deadlineIn time.Duration
	streams    map[*rstream]bool
	idleSince  time.Time
	done       bool
	// opened is true once a stream of the grant was admitted.
	opened bool
}

// execGrant reports whether the grant's operation is an exec.
func (g *grant) execGrant() bool {
	return g.kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_EXEC || g.kind == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_ATTACH_EXEC
}

// admittable: a new first request frame may be accepted.
func (g *grant) admittable(now time.Time, tolerance time.Duration) bool {
	return now.Before(g.received.Add(g.expiresIn+tolerance)) && now.Before(g.received.Add(g.deadlineIn))
}

type rstream struct {
	id      uint64
	g       *grant
	conn    *rconn
	send    *datapath.SendWindow
	recv    *datapath.ReceiveWindow
	ctx     context.Context
	cancel  context.CancelFunc
	started bool
	ended   bool
}

type rconn struct {
	gen      uint64
	out      chan *ownsandv1.DataFrame
	ctx      context.Context
	cancel   context.CancelFunc
	lastRecv atomic.Int64
	lastSend atomic.Int64
	// probeFailed is set before an unanswered echo probe cancels the
	// connection; only then does the loop dial again without back-off.
	probeFailed atomic.Bool
	streams     map[uint64]*rstream
	highest     uint64
	credit      int64
	kinds       map[ownsandv1.DataFrameKind]bool
}

// closeAfterFlush closes the connection after every queued frame was
// sent.
func (c *rconn) closeAfterFlush() {
	select {
	case c.out <- nil:
	case <-c.ctx.Done():
	case <-time.After(5 * time.Second):
		c.cancel()
	}
}

// tryEnqueue offers a frame without waiting and reports whether the
// queue took it. A caller whose frame is optional (a periodic keepalive)
// uses it so it never waits on a writer that only its own cancel can end.
func (c *rconn) tryEnqueue(f *ownsandv1.DataFrame) bool {
	select {
	case c.out <- f:
		return true
	default:
		return false
	}
}

// enqueue queues a frame and waits for a free slot or the end of the
// connection; payload and startup frames use it to keep their order.
func (c *rconn) enqueue(f *ownsandv1.DataFrame) error {
	select {
	case c.out <- f:
		return nil
	case <-c.ctx.Done():
		return errors.New("dataplane: relay connection closed")
	}
}

type relay struct {
	d      *DataPlane
	limits RelayLimits
	client ownsandv1connect.RelayAgentServiceClient

	mu       sync.Mutex
	ch       agent.Channel
	grants   map[string]*grant
	conn     *rconn
	running  bool
	idleFrom time.Time
	wake     chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

// countingBody reports every block of bytes that arrives on the relay
// connection, also of a frame still in transit.
type countingBody struct {
	io.ReadCloser
	note func()
}

func (b countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.note()
	}
	return n, err
}

type countingTransport struct {
	base http.RoundTripper
	note func()
}

func (t countingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err == nil && resp.Body != nil {
		resp.Body = countingBody{ReadCloser: resp.Body, note: t.note}
	}
	return resp, err
}

func newRelay(d *DataPlane) *relay {
	ctx, cancel := context.WithCancel(context.Background())
	r := &relay{d: d, limits: d.opts.Limits.withDefaults(), grants: map[string]*grant{}, wake: make(chan struct{}, 1), ctx: ctx, cancel: cancel}
	if d.opts.HTTP != nil {
		base := d.opts.HTTP.Transport
		if base == nil {
			base = http.DefaultTransport
		}
		hc := &http.Client{Transport: countingTransport{base: base, note: r.noteBytes}}
		r.client = ownsandv1connect.NewRelayAgentServiceClient(hc, d.opts.APIURL, connect.WithReadMaxBytes(datapath.MaxFrameSize+(64<<10)))
	}
	go r.housekeeping()
	return r
}

func (r *relay) noteBytes() {
	r.mu.Lock()
	c := r.conn
	r.mu.Unlock()
	if c != nil {
		c.lastRecv.Store(time.Now().UnixNano())
	}
}

func (r *relay) poke() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *relay) channelOpened(ch agent.Channel) {
	r.mu.Lock()
	r.ch = ch
	r.mu.Unlock()
}

// hasLiveGrantLocked reports an admittable grant of the current channel.
func (r *relay) hasLiveGrantLocked(now time.Time) bool {
	for _, g := range r.grants {
		if r.ch != nil && g.ch.Generation() == r.ch.Generation() && g.admittable(now, r.d.timing.AdmissionTolerance) {
			return true
		}
	}
	return false
}

// grant installs a relay grant and makes sure a relay connection runs.
func (r *relay) grant(ch agent.Channel, pg *ownsandv1.RelayGrant) *ownsandv1.RelayGrantAck {
	now := time.Now()
	logger := r.d.logger.With("grant_id", pg.GetGrantId(), "sandbox_id", pg.GetSandboxId())
	if !uuidv7.Valid(pg.GetGrantId()) || pg.GetOperationKind() == ownsandv1.RelayOperationKind_RELAY_OPERATION_KIND_UNSPECIFIED {
		logger.Warn("ignoring a malformed relay grant")
		return nil
	}
	expiresIn := pg.GetExpiresIn().AsDuration()
	deadlineIn := pg.GetStreamDeadlineIn().AsDuration()
	g := &grant{id: pg.GetGrantId(), sandboxID: pg.GetSandboxId(), kind: pg.GetOperationKind(), opID: pg.GetOperationId(),
		digest: pg.GetRequestDigest(), ch: ch, received: now, expiresIn: expiresIn, deadlineIn: deadlineIn,
		streams: map[*rstream]bool{}, idleSince: now}
	ack := &ownsandv1.RelayGrantAck{GrantId: g.id, Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_ACCEPTED}
	r.mu.Lock()
	if existing, ok := r.grants[g.id]; ok {
		// A repeated grant message: acknowledge the installed grant.
		_ = existing
		r.mu.Unlock()
		return ack
	}
	var replaced *grant
	if old := pg.GetReplacesGrantId(); old != "" {
		if og, ok := r.grants[old]; ok && og.ch.Generation() == ch.Generation() && now.Before(og.received.Add(og.deadlineIn)) {
			replaced = og
			delete(r.grants, old)
			ack.ReplacedGrantId = old
		}
	}
	if replaced == nil && len(r.grants) >= r.limits.MaxGrants {
		r.mu.Unlock()
		logger.Warn("relay grant rejected at the grant limit", "grants", r.limits.MaxGrants)
		return &ownsandv1.RelayGrantAck{GrantId: g.id, Result: ownsandv1.RelayGrantResult_RELAY_GRANT_RESULT_REJECTED_GRANT_LIMIT}
	}
	r.grants[g.id] = g
	r.ch = ch
	conn := r.conn
	start := !r.running
	r.running = true
	r.mu.Unlock()
	if replaced != nil {
		r.endGrantStreams(replaced, ownsandv1.SessionEndReason_SESSION_END_REASON_GRANT_REPLACED)
		logger.Info("relay grant replaced its predecessor", "replaced_grant_id", replaced.id)
	}
	logger.Info("relay grant installed", "operation_kind", g.kind.String(), "expires_in_s", int64(expiresIn.Seconds()))
	if start {
		go r.loop(ch)
	} else if conn != nil {
		r.probe(conn)
	}
	r.poke()
	return ack
}

// probe checks a connection that received nothing for ProbeIdle with an
// echo request; without any byte within ProbeTimeout it is treated as lost
// and dialed again at once.
func (r *relay) probe(c *rconn) {
	last := time.Unix(0, c.lastRecv.Load())
	if time.Since(last) < r.d.timing.ProbeIdle {
		return
	}
	sent := time.Now()
	if c.enqueue(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE, Payload: &ownsandv1.DataFrame_RelayKeepalive{RelayKeepalive: &ownsandv1.RelayKeepalive{EchoRequested: true}}}) != nil {
		return
	}
	go func() {
		select {
		case <-c.ctx.Done():
		case <-time.After(r.d.timing.ProbeTimeout):
			if time.Unix(0, c.lastRecv.Load()).Before(sent) {
				r.d.logger.Warn("relay connection did not answer the echo probe; reconnecting")
				c.probeFailed.Store(true)
				c.cancel()
			}
		}
	}()
}

// loop keeps one relay connection while the current channel has a live
// grant. It follows r.ch: a grant of a new channel that arrives while the
// loop still serves the previous channel does not start a second loop, so
// this loop dials for the new channel next. The decision to stop and the
// reset of r.running happen under one lock, so a grant that arrives
// meanwhile starts a new loop.
func (r *relay) loop(ch agent.Channel) {
	b := backoff.Backoff{Min: 200 * time.Millisecond, Max: r.d.timing.ReconnectMax}
	for {
		r.mu.Lock()
		cur := r.ch
		closed := cur == nil
		if !closed {
			select {
			case <-cur.Done():
				closed = true
			case <-r.ctx.Done():
				closed = true
			default:
			}
		}
		if closed || !r.hasLiveGrantLocked(time.Now()) {
			r.running = false
			r.mu.Unlock()
			return
		}
		r.mu.Unlock()
		if cur != ch {
			r.d.logger.Info("relay connection follows the new agent channel", "generation", cur.Generation())
			ch = cur
			b.Reset()
		}
		probeLost, err := r.connectOnce(ch)
		switch {
		case errors.Is(err, errIdle), errors.Is(err, errChannelChanged):
			continue
		case err != nil && !probeLost:
			r.d.logger.Warn("relay connection ended", "error", err)
			if backoff.Sleep(r.ctx, b.Next()) != nil {
				r.mu.Lock()
				r.running = false
				r.mu.Unlock()
				return
			}
		case probeLost:
			r.d.logger.Warn("relay connection lost; dialing again", "error", err)
			b.Reset()
		}
	}
}

var (
	errIdle           = errors.New("dataplane: relay connection idle")
	errChannelChanged = errors.New("dataplane: agent channel changed before the relay connection opened")
)

// connectOnce runs one relay connection until it ends. It reports whether
// the echo probe ended it.
func (r *relay) connectOnce(ch agent.Channel) (bool, error) {
	if r.client == nil {
		return false, errors.New("dataplane: no relay client")
	}
	cred, err := r.d.opts.Auth.NewDeviceSession(r.ctx)
	if err != nil {
		return false, err
	}
	// The connection ends through channelLost, which first queues the
	// session-end frames of every stream and then closes the connection
	// after they were sent.
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	c := &rconn{gen: ch.Generation(), out: make(chan *ownsandv1.DataFrame, 256), ctx: ctx, cancel: cancel,
		streams: map[uint64]*rstream{}, kinds: map[ownsandv1.DataFrameKind]bool{}}
	now := time.Now().UnixNano()
	c.lastRecv.Store(now)
	c.lastSend.Store(now)
	// The channel can change while the credential is fetched. A
	// connection of a replaced channel would serve no grant, and
	// channelLost closes only the installed connection, so the loop
	// dials again for the current channel instead.
	r.mu.Lock()
	if r.ch != ch {
		r.mu.Unlock()
		return false, errChannelChanged
	}
	r.conn = c
	r.idleFrom = time.Now()
	r.mu.Unlock()
	defer r.connClosed(c)
	stream := r.client.Connect(ctx)
	cpclient.SetBearer(stream.RequestHeader(), cred)
	guestclient.CloseOnDone(ctx, stream)

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case f := <-c.out:
				if f == nil {
					// The flush marker of closeAfterFlush: end the request
					// side so the queued frames reach the control plane
					// before the stream is canceled.
					_ = stream.CloseRequest()
					time.AfterFunc(2*time.Second, cancel)
					return
				}
				if err := stream.Send(&ownsandv1.ConnectRequest{Frame: f}); err != nil {
					cancel()
					return
				}
				c.lastSend.Store(time.Now().UnixNano())
			}
		}
	}()
	// The request headers go out with an initial keepalive at once, then
	// the supported frame kinds.
	c.enqueue(keepaliveFrame(false))
	c.enqueue(&ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_SUPPORTED_FRAME_KINDS,
		Payload: &ownsandv1.DataFrame_SupportedFrameKinds{SupportedFrameKinds: &ownsandv1.SupportedFrameKinds{Kinds: datapath.AllKinds()}}})
	r.d.logger.Info("relay connection open", "transport", ownsandv1.DataPathTransport_DATA_PATH_TRANSPORT_RELAY.String())

	recvErr := make(chan error, 1)
	go func() {
		for {
			resp, err := stream.Receive()
			if err != nil {
				recvErr <- err
				return
			}
			c.lastRecv.Store(time.Now().UnixNano())
			if f := resp.GetFrame(); f != nil {
				r.onFrame(c, f)
			}
		}
	}()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case err := <-recvErr:
			// This cancel only cleans up, so it does not count as a probe
			// loss: an ordinary connection error keeps the back-off.
			cancel()
			<-writerDone
			return c.probeFailed.Load() && r.ctx.Err() == nil, err
		case <-ctx.Done():
			awaitTeardown(writerDone, recvErr)
			return c.probeFailed.Load() && r.ctx.Err() == nil, errors.New("dataplane: relay connection canceled")
		case <-tick.C:
			now := time.Now()
			if r.watchdogTick(c, now) {
				cancel()
				awaitTeardown(writerDone, recvErr)
				return true, errors.New("dataplane: relay connection received nothing within the receive timeout")
			}
			r.mu.Lock()
			if r.hasLiveGrantLocked(now) || len(c.streams) > 0 {
				r.idleFrom = now
			}
			idle := now.Sub(r.idleFrom) >= r.d.timing.ConnectionIdle
			r.mu.Unlock()
			if idle {
				cancel()
				awaitTeardown(writerDone, recvErr)
				r.d.logger.Info("relay connection closed after its idle time")
				return false, errIdle
			}
		}
	}
}

// teardownGrace bounds how long a canceled relay connection waits for its
// writer and receive goroutines to end.
//
// Workaround: when the writer is blocked by HTTP/2 flow control, net/http
// does not abort the stream on context cancellation while the request body
// is still open, so the receive goroutine stays in its read until the
// transport itself fails. Without a bound, the loop would wait on that
// goroutine and never dial again, which defeats the receive timeout. The
// receive goroutine reports into a buffered channel, so it ends alone
// when the transport fails. Remove the bound when net/http aborts such a
// stream on context cancellation.
const teardownGrace = 2 * time.Second

// awaitTeardown waits for the writer and receive goroutines of a canceled
// relay connection, for at most teardownGrace.
func awaitTeardown(writerDone <-chan struct{}, recvErr <-chan error) {
	timer := time.NewTimer(teardownGrace)
	defer timer.Stop()
	for writerDone != nil || recvErr != nil {
		select {
		case <-writerDone:
			writerDone = nil
		case <-recvErr:
			recvErr = nil
		case <-timer.C:
			return
		}
	}
}

// watchdogTick runs the periodic checks of one relay connection and
// reports whether the receive timeout expired. It checks the receive
// timeout first and never waits on the outbound queue: when a blocked
// writer keeps the queue full, only this watchdog can cancel the
// connection, so a blocking keepalive enqueue would deadlock it. A
// periodic keepalive that finds no free slot is skipped, because the
// queued frames already show the connection is in use and the next tick
// offers it again.
func (r *relay) watchdogTick(c *rconn, now time.Time) bool {
	if now.Sub(time.Unix(0, c.lastRecv.Load())) >= r.d.timing.ReceiveTimeout {
		return true
	}
	if now.Sub(time.Unix(0, c.lastSend.Load())) >= r.d.timing.KeepaliveInterval {
		if !c.tryEnqueue(keepaliveFrame(false)) {
			r.d.logger.Debug("relay keepalive skipped because the outbound queue is full")
		}
	}
	return false
}

func keepaliveFrame(echo bool) *ownsandv1.DataFrame {
	return &ownsandv1.DataFrame{Kind: ownsandv1.DataFrameKind_DATA_FRAME_KIND_RELAY_KEEPALIVE,
		Payload: &ownsandv1.DataFrame_RelayKeepalive{RelayKeepalive: &ownsandv1.RelayKeepalive{EchoRequested: echo}}}
}

// connClosed ends every stream of a closed connection, as for a relay
// stream close frame; exec and file operations keep their own retention.
func (r *relay) connClosed(c *rconn) {
	r.mu.Lock()
	if r.conn == c {
		r.conn = nil
	}
	streams := make([]*rstream, 0, len(c.streams))
	for _, st := range c.streams {
		streams = append(streams, st)
	}
	r.mu.Unlock()
	for _, st := range streams {
		r.finishStream(st, nil)
	}
}
